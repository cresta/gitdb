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

	"github.com/cresta/gitdb/internal/httpserver"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/cresta/gitdb/internal/log"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type Config struct {
	DataDirectory    string
	Repos            string
	PrivateKey       string
	PrivateKeyPasswd string
}

func NewHandler(logger *log.Logger, cfg Config) (*CheckoutHandler, error) {
	logger.Info(context.Background(), "setting up git server")
	publicKeys, err := getPrivateKeys(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to load private key: %w", err)
	}
	g := GitOperator{
		Log: logger,
	}
	dataDir := cfg.DataDirectory
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	repos := strings.Split(cfg.Repos, ",")
	gitCheckouts := make(map[string]*GitCheckout)
	ctx := context.Background()
	for idx, repo := range repos {
		repo := strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		cloneInto, err := ioutil.TempDir(dataDir, "gitdb_repo_"+sanitizeDir(repo))
		if err != nil {
			return nil, fmt.Errorf("unable to make temp dir for %s,%s: %w", dataDir, "gitdb_repo_"+sanitizeDir(repo), err)
		}
		co, err := g.Clone(ctx, cloneInto, repo, getPublicKey(publicKeys, idx))
		if err != nil {
			return nil, fmt.Errorf("unable to clone repo %s: %w", repo, err)
		}
		gitCheckouts[getRepoKey(repo)] = co
		logger.Info(context.Background(), "setup checkout", zap.String("repo", repo), zap.String("key", getRepoKey(repo)))
	}
	ret := &CheckoutHandler{
		Checkouts: gitCheckouts,
		Log:       logger.With(zap.String("class", "checkout_handler")),
	}
	return ret, nil
}

type CheckoutHandler struct {
	Checkouts map[string]*GitCheckout
	Log       *log.Logger
}

func (h *CheckoutHandler) CheckoutsByRepo() map[string]*GitCheckout {
	ret := make(map[string]*GitCheckout)
	for _, c := range h.Checkouts {
		ret[c.remoteURL] = c
	}
	return ret
}

func (h *CheckoutHandler) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodGet).Path("/file/{repo}/{branch}/{path:.*}").Handler(httpserver.BasicHandler(h.getFileHandler, h.Log)).Name("get_file_handler")
	mux.Methods(http.MethodGet).Path("/ls/{repo}/{branch}/{dir:.*}").Handler(httpserver.BasicHandler(h.lsDirHandler, h.Log)).Name("ls_dir_handler")
	mux.Methods(http.MethodPost).Path("/refresh/{repo}").Handler(httpserver.BasicHandler(h.refreshRepoHandler, h.Log)).Name("refresh_repo")
	mux.Methods(http.MethodPost).Path("/refreshall").Handler(httpserver.BasicHandler(h.refreshAllRepoHandler, h.Log)).Name("refresh_all")
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
	logger.Info(req.Context(), "get file handler")
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
	logger.Info(req.Context(), "ls dir handler")
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
		logger.Info(req.Context(), "invalid repo")
		return &httpserver.BasicResponse{Code: http.StatusNotFound, Msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(req.Context(), branchAsRef.String())
	if err != nil {
		logger.Info(req.Context(), "invalid branch", zap.Error(err))
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
		logger.Info(ctx, "invalid repo")
		return &httpserver.BasicResponse{Code: http.StatusNotFound, Msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(ctx, branchAsRef.String())
	if err != nil {
		logger.Info(ctx, "invalid branch", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusNotFound,
			Msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	f, err := r.FileContent(ctx, path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			logger.Info(ctx, "File does not exist", zap.Error(err))
			return &httpserver.BasicResponse{
				Code: http.StatusNotFound,
				Msg:  strings.NewReader(fmt.Sprintf("unable to find file %s in branch %s for repo %s", path, branch, repo)),
			}
		}
		logger.Info(ctx, "internal server error", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("Unable to fetch file %s: %s", path, err)),
		}
	}
	logger.Info(ctx, "fetch ok")
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

func getPublicKey(k []transport.AuthMethod, idx int) transport.AuthMethod {
	if len(k) == 0 {
		return nil
	}
	if len(k) == 1 {
		return k[0]
	}
	if idx >= len(k) {
		return nil
	}
	return k[idx]
}

func getPrivateKeys(cfg Config) ([]transport.AuthMethod, error) {
	pKey := strings.TrimSpace(cfg.PrivateKey)
	if pKey == "" {
		return nil, nil
	}
	files := strings.Split(pKey, ",")
	ret := make([]transport.AuthMethod, 0, len(files))
	for _, file := range files {
		if file == "" {
			ret = append(ret, nil)
		}
		sshKey, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("unable to read file %s: %w", file, err)
		}
		publicKey, err := ssh.NewPublicKeys("git", sshKey, cfg.PrivateKeyPasswd)
		if err != nil {
			return nil, fmt.Errorf("unable to load public keys: %w", err)
		}
		ret = append(ret, publicKey)
	}
	return ret, nil
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
