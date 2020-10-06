package gitdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cresta/gitdb/internal/log"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

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

type CoreMux interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

var _ CoreMux = http.NewServeMux()

func (h *CheckoutHandler) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodGet).Path("/file/{repo}/{branch}/{path:.*}").HandlerFunc(h.getFileHandler)
	mux.Methods(http.MethodPost).Path("/refresh/{repo}").HandlerFunc(h.refreshRepoHandler)
	mux.Methods(http.MethodPost).Path("/refreshall").HandlerFunc(h.refreshAllRepoHandler)
}

type getFileResp struct {
	code int
	msg  io.WriterTo
}

type CanHTTPWrite interface {
	HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger)
}

var _ CanHTTPWrite = &getFileResp{}

func (g *getFileResp) HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger) {
	w.WriteHeader(g.code)
	if w != nil {
		_, err := g.msg.WriteTo(w)
		l.IfErr(err).Error(ctx, "unable to write final object")
	}
}

func (h *CheckoutHandler) genericHandler(ctx context.Context, resp CanHTTPWrite, w http.ResponseWriter, l *log.Logger) {
	resp.HTTPWrite(ctx, w, l)
}

func (h *CheckoutHandler) refreshAllRepoHandler(w http.ResponseWriter, req *http.Request) {
	logger := h.Log.With(zap.String("handler", "all_repo"))
	for repoName, repo := range h.Checkouts {
		if err := repo.Refresh(req.Context()); err != nil {
			h.genericHandler(req.Context(), &getFileResp{
				code: http.StatusInternalServerError,
				msg:  strings.NewReader(fmt.Sprintf("unable to refresh %s: %v", repoName, err)),
			}, w, logger.With(zap.String("repo", repoName)))
			return
		}
	}
	h.genericHandler(req.Context(), &getFileResp{
		code: http.StatusOK,
		msg:  strings.NewReader("OK"),
	}, w, logger)
}

func (h *CheckoutHandler) refreshRepoHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repo := vars["repo"]
	logger := h.Log.With(zap.String("repo", repo))
	r, exists := h.Checkouts[repo]
	if !exists {
		h.genericHandler(req.Context(), &getFileResp{
			code: http.StatusNotFound,
			msg:  strings.NewReader(fmt.Sprintf("unknown repo %s", repo)),
		}, w, logger)
		return
	}
	err := r.Refresh(req.Context())
	if err != nil {
		h.genericHandler(req.Context(), &getFileResp{
			code: http.StatusInternalServerError,
			msg:  strings.NewReader(fmt.Sprintf("unable to fetch remote content %s", err)),
		}, w, logger)
		return
	}
	h.genericHandler(req.Context(), &getFileResp{
		code: http.StatusOK,
		msg:  strings.NewReader("OK"),
	}, w, logger)
}

func (h *CheckoutHandler) getFileHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repo := vars["repo"]
	branch := vars["branch"]
	path := vars["path"]
	logger := h.Log.With(zap.String("repo", repo), zap.String("branch", branch), zap.String("path", path))
	logger.Info(req.Context(), "get file handler")
	if repo == "" || branch == "" || path == "" {
		w.WriteHeader(http.StatusNotFound)
		if _, err := fmt.Fprintf(w, "One unset{repo: %s, branch: %s, path: %s}", repo, branch, path); err != nil {
			logger.Warn(req.Context(), "unable to find repo/branch/path")
		}
		return
	}
	h.genericHandler(req.Context(), h.getFile(req.Context(), repo, branch, path, logger), w, logger)
}

func (h *CheckoutHandler) getFile(ctx context.Context, repo string, branch string, path string, logger *log.Logger) *getFileResp {
	r, exists := h.Checkouts[repo]
	if !exists {
		buf := strings.NewReader(fmt.Sprintf("unable to find repo %s", repo))
		logger.Info(ctx, "invalid repo")
		return &getFileResp{code: http.StatusNotFound, msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(ctx, branchAsRef.String())
	if err != nil {
		logger.Info(ctx, "invalid branch", zap.Error(err))
		return &getFileResp{
			code: http.StatusNotFound,
			msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	f, err := r.FileContent(ctx, path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			logger.Info(ctx, "File does not exist", zap.Error(err))
			return &getFileResp{
				code: http.StatusNotFound,
				msg:  strings.NewReader(fmt.Sprintf("unable to find file %s in branch %s for repo %s", path, branch, repo)),
			}
		}
		logger.Info(ctx, "internal server error", zap.Error(err))
		return &getFileResp{
			code: http.StatusInternalServerError,
			msg:  strings.NewReader(fmt.Sprintf("Unable to fetch file %s: %s", path, err)),
		}
	}
	logger.Info(ctx, "fetch ok")
	return &getFileResp{
		code: http.StatusOK,
		msg:  f,
	}
}
