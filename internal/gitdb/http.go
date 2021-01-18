package gitdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	jwtmiddleware "github.com/auth0/go-jwt-middleware"

	"github.com/cresta/gitdb/internal/gitdb/tracing"
	"github.com/cresta/gitdb/internal/httpserver"
	"github.com/cresta/gitdb/internal/log"
	"github.com/dgrijalva/jwt-go"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type Config struct {
	DataDirectory string
	Repos         []Repository
}

type Repository struct {
	URL                    string
	PrivateKey             string
	PrivateKeyPassword     string
	PrivateKeyPasswordFile string
	Alias                  string
	Public                 bool
}

func NewHandler(logger *log.Logger, cfg Config, tracer tracing.Tracing) (*CheckoutHandler, error) {
	logger.Info(context.Background(), "setting up git server")
	g := GitOperator{
		Log:    logger,
		Tracer: tracer,
	}
	dataDir := cfg.DataDirectory
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	gitCheckouts := make(map[string]*GitCheckout)
	checkoutConfigs := make(map[string]Repository)
	ctx := context.Background()
	for idx, repo := range cfg.Repos {
		trimmedRepoURL := strings.TrimSpace(repo.URL)
		if trimmedRepoURL == "" {
			return nil, fmt.Errorf("unable to find URL for repo index %d", idx)
		}
		cloneInto, err := ioutil.TempDir(dataDir, "gitdb_repo_"+sanitizeDir(trimmedRepoURL))
		if err != nil {
			return nil, fmt.Errorf("unable to make temp dir for %s,%s: %w", dataDir, "gitdb_repo_"+sanitizeDir(trimmedRepoURL), err)
		}
		authMethod, err := getAuthMethod(repo)
		if err != nil {
			return nil, fmt.Errorf("unable to load private key: %w", err)
		}
		co, err := g.Clone(ctx, cloneInto, trimmedRepoURL, authMethod)
		if err != nil {
			return nil, fmt.Errorf("unable to clone repo %s: %w", trimmedRepoURL, err)
		}
		repoKey := repo.Alias
		if repoKey == "" {
			repoKey = getRepoKey(trimmedRepoURL)
		}
		gitCheckouts[repoKey] = co
		checkoutConfigs[repoKey] = repo
		logger.Info(context.Background(), "setup checkout", zap.String("repo", trimmedRepoURL), zap.String("key", repoKey), zap.String("into", cloneInto))
	}
	logger.Info(context.Background(), "repos loaded", zap.Int("num_keys", len(cfg.Repos)))
	ret := &CheckoutHandler{
		Checkouts:       gitCheckouts,
		checkoutConfigs: checkoutConfigs,
		Log:             logger.With(zap.String("class", "checkout_handler")),
	}
	return ret, nil
}

type CheckoutHandler struct {
	Checkouts       map[string]*GitCheckout
	Log             *log.Logger
	checkoutConfigs map[string]Repository
}

func (h *CheckoutHandler) CheckoutsByRepo() map[string]*GitCheckout {
	ret := make(map[string]*GitCheckout)
	for _, c := range h.Checkouts {
		ret[c.remoteURL] = c
	}
	return ret
}

func (h *CheckoutHandler) SetupPublicJWTHandler(muxRouter *mux.Router, keyFunc jwt.Keyfunc, repos []Repository) {
	if noPublicRepos(repos) {
		return
	}
	middleware := jwtmiddleware.New(jwtmiddleware.Options{
		ValidationKeyGetter: keyFunc,
		SigningMethod:       jwt.SigningMethodRS256,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err string) {
			resp := httpserver.BasicResponse{
				Code:    http.StatusUnauthorized,
				Msg:     strings.NewReader(err),
				Headers: nil,
			}
			h.Log.Warn(r.Context(), "error during JWT", zap.String("err_string", err))
			resp.HTTPWrite(r.Context(), w, h.Log)
		},
	})
	publicRepoMiddleware := func(root http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			vars := mux.Vars(request)
			repo := vars["repo"]
			if repoCfg, exists := h.checkoutConfigs[repo]; !exists {
				writer.WriteHeader(http.StatusNotFound)
				return
			} else if !repoCfg.Public {
				h.Log.Warn(request.Context(), "attempting to fetch private repo from public endpoint", zap.String("repo", repo))
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			root.ServeHTTP(writer, request)
		})
	}

	muxRouter.Methods(http.MethodGet).Path("/public/file/{repo}/{branch}/{path:.*}").Handler(publicRepoMiddleware(middleware.Handler(httpserver.BasicHandler(h.getFileHandler, h.Log)))).Name("public_get_file_handler")
	muxRouter.Methods(http.MethodGet).Path("/public/ls/{repo}/{branch}/{dir:.*}").Handler(publicRepoMiddleware(middleware.Handler(httpserver.BasicHandler(h.lsDirHandler, h.Log)))).Name("public_ls_dir_handler")
	muxRouter.Methods(http.MethodGet).Path("/public/zip/{repo}/{branch}/{dir:.*}").Handler(publicRepoMiddleware(middleware.Handler(httpserver.BasicHandler(h.zipDirHandler, h.Log)))).Name("public_zip_dir_handler")
	muxRouter.Methods(http.MethodGet).Path("/refresh/{repo}").Handler(publicRepoMiddleware(middleware.Handler(httpserver.BasicHandler(h.refreshRepoHandler, h.Log)))).Name("refresh_repo")
	muxRouter.Methods(http.MethodGet).Path("/refreshall").Handler(middleware.Handler(httpserver.BasicHandler(h.refreshAllRepoHandler, h.Log)))).Name("refresh_all")
}

func noPublicRepos(repos []Repository) bool {
	for _, repo := range repos {
		if repo.Public {
			return false
		}
	}
	return true
}

func (h *CheckoutHandler) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodGet).Path("/file/{repo}/{branch}/{path:.*}").Handler(httpserver.BasicHandler(h.getFileHandler, h.Log)).Name("get_file_handler")
	mux.Methods(http.MethodGet).Path("/ls/{repo}/{branch}/{dir:.*}").Handler(httpserver.BasicHandler(h.lsDirHandler, h.Log)).Name("ls_dir_handler")
	mux.Methods(http.MethodGet).Path("/zip/{repo}/{branch}/{dir:.*}").Handler(httpserver.BasicHandler(h.zipDirHandler, h.Log)).Name("zip_dir_handler")
	mux.Methods(http.MethodGet).Path("/refresh/{repo}").Handler(httpserver.BasicHandler(h.refreshRepoHandler, h.Log)).Name("refresh_repo")
	mux.Methods(http.MethodGet).Path("/refreshall").Handler(httpserver.BasicHandler(h.refreshAllRepoHandler, h.Log)).Name("refresh_all")
}

func (h *CheckoutHandler) refreshAllRepoHandler(req *http.Request) httpserver.CanHTTPWrite {
	for repoName, repo := range h.Checkouts {
		if err := repo.Refresh(req.Context()); err != nil {
			return &httpserver.BasicResponse{
				Code: http.StatusInternalServerError,
				Msg:  strings.NewReader(fmt.Sprintf("unable to refresh %s: %v", repoName, err)),
			}
		}
	}
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  strings.NewReader("OK"),
	}
}

func (h *CheckoutHandler) refreshRepoHandler(req *http.Request) httpserver.CanHTTPWrite {
	vars := mux.Vars(req)
	repo := vars["repo"]
	r, exists := h.Checkouts[repo]
	if !exists {
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("unknown repo %s", repo)),
		}
	}
	err := r.Refresh(req.Context())
	if err != nil {
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("unable to fetch remote content %s", err)),
		}
	}
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  strings.NewReader("OK"),
	}
}

func (h *CheckoutHandler) getFileHandler(req *http.Request) httpserver.CanHTTPWrite {
	vars := mux.Vars(req)
	repo := vars["repo"]
	branch := vars["branch"]
	path := vars["path"]
	logger := h.Log.With(zap.String("repo", repo), zap.String("branch", branch), zap.String("path", path))
	logger.Debug(req.Context(), "get file handler")
	if repo == "" || branch == "" || path == "" {
		logger.Warn(req.Context(), "unable to find repo/branch/path")
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("One unset{repo: %s, branch: %s, path: %s}", repo, branch, path)),
		}
	}
	return h.getFile(req.Context(), repo, branch, path, logger)
}

func (h *CheckoutHandler) lsDirHandler(req *http.Request) httpserver.CanHTTPWrite {
	vars := mux.Vars(req)
	repo := vars["repo"]
	branch := vars["branch"]
	dir := vars["dir"]
	logger := h.Log.With(zap.String("repo", repo), zap.String("branch", branch), zap.String("dir", dir))
	logger.Debug(req.Context(), "ls dir handler")
	if repo == "" || branch == "" {
		logger.Warn(req.Context(), "unable to find repo/branch")
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("One unset{repo: %s, branch: %s}", repo, branch)),
		}
	}
	r, exists := h.Checkouts[repo]
	if !exists {
		buf := strings.NewReader(fmt.Sprintf("unable to find repo %s", repo))
		logger.Warn(req.Context(), "invalid repo")
		return &httpserver.BasicResponse{Code: http.StatusNotFound, Msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(req.Context(), branchAsRef.String())
	if err != nil {
		logger.Warn(req.Context(), "invalid branch", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	stat, err := r.LsDir(req.Context(), dir)
	if err != nil {
		if errors.Is(err, object.ErrDirectoryNotFound) {
			return &httpserver.BasicResponse{
				Code: http.StatusNotFound,
				Msg:  strings.NewReader(fmt.Sprintf("directory not found %s", dir)),
			}
		}
		logger.Warn(req.Context(), "unable to list path", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("unable to list path %s: %v", dir, err)),
		}
	}
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  FileStatArr(stat),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
}

func (h *CheckoutHandler) zipDirHandler(req *http.Request) httpserver.CanHTTPWrite {
	vars := mux.Vars(req)
	repo := vars["repo"]
	branch := vars["branch"]
	dir := vars["dir"]
	logger := h.Log.With(zap.String("repo", repo), zap.String("branch", branch), zap.String("dir", dir))
	logger.Debug(req.Context(), "ls dir handler")
	if repo == "" || branch == "" {
		logger.Warn(req.Context(), "unable to find repo/branch")
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("One unset{repo: %s, branch: %s}", repo, branch)),
		}
	}
	r, exists := h.Checkouts[repo]
	if !exists {
		buf := strings.NewReader(fmt.Sprintf("unable to find repo %s", repo))
		logger.Warn(req.Context(), "invalid repo")
		return &httpserver.BasicResponse{Code: http.StatusNotFound, Msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(req.Context(), branchAsRef.String())
	if err != nil {
		logger.Warn(req.Context(), "invalid branch", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	var buf bytes.Buffer
	if numFiles, err := ZipContent(req.Context(), &buf, dir, r); err != nil {
		logger.Warn(req.Context(), "unable to zip content", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("unable to zip content for %s: %v", dir, err)),
		}
	} else if numFiles == 0 {
		logger.Warn(req.Context(), "no files in path")
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("no files in path %s", dir)),
		}
	}
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  &buf,
		Headers: map[string]string{
			"Content-Type": "application/zip",
		},
	}
}

type FileStatArr []FileStat

func (f FileStatArr) WriteTo(w io.Writer) (int64, error) {
	var b bytes.Buffer
	err := json.NewEncoder(&b).Encode(f)
	if err != nil {
		return 0, fmt.Errorf("unable to encode body: %w", err)
	}
	return io.Copy(w, &b)
}

func (h *CheckoutHandler) getFile(ctx context.Context, repo string, branch string, path string, logger *log.Logger) httpserver.CanHTTPWrite {
	r, exists := h.Checkouts[repo]
	if !exists {
		buf := strings.NewReader(fmt.Sprintf("unable to find repo %s", repo))
		logger.Warn(ctx, "invalid repo")
		return &httpserver.BasicResponse{Code: http.StatusNotFound, Msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(ctx, branchAsRef.String())
	if err != nil {
		logger.Warn(ctx, "invalid branch", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	f, err := r.FileContent(ctx, path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			logger.Warn(ctx, "File does not exist", zap.Error(err))
			return &httpserver.BasicResponse{
				Code: http.StatusNotFound,
				Msg:  strings.NewReader(fmt.Sprintf("unable to find file %s in branch %s for repo %s", path, branch, repo)),
			}
		}
		logger.Warn(ctx, "internal server error", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("Unable to fetch file %s: %s", path, err)),
		}
	}
	logger.Debug(ctx, "fetch ok")
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  f,
	}
}

func sanitizeDir(s string) string {
	allowed := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890-"
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(allowed, r) {
			return r
		}
		return '_'
	}, s)
}

func getAuthMethod(repo Repository) (transport.AuthMethod, error) {
	pKey := strings.TrimSpace(repo.PrivateKey)
	if pKey == "" {
		return nil, nil
	}
	sshKey, err := ioutil.ReadFile(pKey)
	if err != nil {
		return nil, fmt.Errorf("unable to read file %s: %w", pKey, err)
	}
	publicKey, err := ssh.NewPublicKeys("git", sshKey, repo.PrivateKeyPassword)
	if err != nil {
		return nil, fmt.Errorf("unable to load public keys: %w", err)
	}
	return publicKey, nil
}

func getRepoKey(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return repo
	}
	parts2 := strings.Split(parts[1], ".")
	if len(parts2) != 2 {
		return repo
	}
	return parts2[0]
}
