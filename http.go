package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type checkoutHandler struct {
	checkouts map[string]*gitCheckout
	log       *zap.Logger
	mux       *mux.Router
}

func (h *checkoutHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.mux.ServeHTTP(w, req)
}

func (h *checkoutHandler) setupMux() {
	if h.mux != nil {
		panic("do not call setup twice")
	}
	h.mux = mux.NewRouter()
	h.mux.Methods(http.MethodGet).Path("/file/{repo}/{branch}/{path:.*}").HandlerFunc(h.getFileHandler)
	h.mux.Methods(http.MethodPost).Path("/refresh/{repo}").HandlerFunc(h.refreshRepoHandler)
	h.mux.Methods(http.MethodPost).Path("/refresh").HandlerFunc(h.refreshAllRepoHandler)
	h.mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		h.log.Info("not found handler")
		(&ContextZapLogger{h.log}).With(req.Context()).With(zap.String("handler", "not_found"), zap.String("url", req.URL.String())).Warn("unknown request")
		http.NotFoundHandler().ServeHTTP(rw, req)
	})
}

var _ http.Handler = &checkoutHandler{}

type getFileResp struct {
	code int
	msg  io.WriterTo
}

type CanHTTPWrite interface {
	HTTPWrite(w http.ResponseWriter, l *zap.Logger)
}

var _ CanHTTPWrite = &getFileResp{}

func (g *getFileResp) HTTPWrite(w http.ResponseWriter, l *zap.Logger) {
	w.WriteHeader(g.code)
	if w != nil {
		if _, err := g.msg.WriteTo(w); err != nil {
			l.Error("unable to write final object", zap.Error(err))
		}
	}
}

func (h *checkoutHandler) genericHandler(resp CanHTTPWrite, w http.ResponseWriter, l *zap.Logger) {
	resp.HTTPWrite(w, l)
}

func (h *checkoutHandler) refreshAllRepoHandler(w http.ResponseWriter, req *http.Request) {
	logger := h.log.With(zap.String("handler", "all_repo"))
	for repoName, repo := range h.checkouts {
		if err := repo.Refresh(req.Context()); err != nil {
			h.genericHandler(&getFileResp{
				code: http.StatusInternalServerError,
				msg:  strings.NewReader(fmt.Sprintf("unable to refresh %s: %v", repoName, err)),
			}, w, logger.With(zap.String("repo", repoName)))
			return
		}
	}
	h.genericHandler(&getFileResp{
		code: http.StatusOK,
		msg:  strings.NewReader("OK"),
	}, w, logger)
}

func (h *checkoutHandler) refreshRepoHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repo := vars["repo"]
	logger := h.log.With(zap.String("repo", repo))
	r, exists := h.checkouts[repo]
	if !exists {
		h.genericHandler(&getFileResp{
			code: http.StatusNotFound,
			msg:  strings.NewReader(fmt.Sprintf("unknown repo %s", repo)),
		}, w, logger)
		return
	}
	err := r.Refresh(req.Context())
	if err != nil {
		h.genericHandler(&getFileResp{
			code: http.StatusInternalServerError,
			msg:  strings.NewReader(fmt.Sprintf("unable to fetch remote content %s", err)),
		}, w, logger)
		return
	}
	h.genericHandler(&getFileResp{
		code: http.StatusOK,
		msg:  strings.NewReader("OK"),
	}, w, logger)
}

func (h *checkoutHandler) getFileHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	repo := vars["repo"]
	branch := vars["branch"]
	path := vars["path"]
	logger := h.log.With(zap.String("repo", repo), zap.String("branch", branch), zap.String("path", path))
	logger.Info("get file handler")
	if repo == "" || branch == "" || path == "" {
		w.WriteHeader(http.StatusNotFound)
		if _, err := fmt.Fprintf(w, "One unset{repo: %s, branch: %s, path: %s}", repo, branch, path); err != nil {
			logger.Warn("unable to find repo/branch/path")
		}
		return
	}
	h.genericHandler(h.getFile(repo, branch, path, logger), w, logger)
}

func (h *checkoutHandler) getFile(repo string, branch string, path string, logger *zap.Logger) *getFileResp {
	r, exists := h.checkouts[repo]
	if !exists {
		buf := strings.NewReader(fmt.Sprintf("unable to find repo %s", repo))
		logger.Info("invalid repo")
		return &getFileResp{code: http.StatusNotFound, msg: buf}
	}
	branchAsRef := plumbing.NewRemoteReferenceName("origin", branch)
	r, err := r.WithReference(branchAsRef.String())
	if err != nil {
		logger.Info("invalid branch", zap.Error(err))
		return &getFileResp{
			code: http.StatusNotFound,
			msg:  strings.NewReader(fmt.Sprintf("unable to find branch %s for repo %s", branch, repo)),
		}
	}
	f, err := r.FileContent(path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			logger.Info("File does not exist", zap.Error(err))
			return &getFileResp{
				code: http.StatusNotFound,
				msg:  strings.NewReader(fmt.Sprintf("unable to find file %s in branch %s for repo %s", path, branch, repo)),
			}
		}
		logger.Info("internal server error", zap.Error(err))
		return &getFileResp{
			code: http.StatusInternalServerError,
			msg:  strings.NewReader(fmt.Sprintf("Unable to fetch file %s: %s", path, err)),
		}
	}
	logger.Info("fetch ok")
	return &getFileResp{
		code: http.StatusOK,
		msg:  f,
	}
}
