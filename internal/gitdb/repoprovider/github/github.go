package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cresta/gitdb/internal/log"

	"github.com/google/go-github/v32/github"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type GitCheckout interface {
	Refresh(ctx context.Context) error
}

type Provider struct {
	Token     []byte
	Logger    *log.Logger
	Checkouts map[string]GitCheckout
}

type pushEventResponse struct {
	code int
	msg  io.WriterTo
}

type CanHTTPWrite interface {
	HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger)
}

var _ CanHTTPWrite = &pushEventResponse{}

func (g *pushEventResponse) HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger) {
	w.WriteHeader(g.code)
	if w != nil {
		_, err := g.msg.WriteTo(w)
		l.IfErr(err).Error(ctx, "unable to write final object")
	}
}

func (p *Provider) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodPost).Path("/public/github/push_event").HandlerFunc(p.PushEventHandler).Name("push_event")
}

func (p *Provider) genericHandler(ctx context.Context, resp CanHTTPWrite, w http.ResponseWriter, l *log.Logger) {
	resp.HTTPWrite(ctx, w, l)
}

func (p *Provider) PushEventHandler(w http.ResponseWriter, req *http.Request) {
	p.genericHandler(req.Context(), p.pushEvent(req), w, p.Logger)
}

func (p *Provider) pushEvent(req *http.Request) *pushEventResponse {
	p.Logger.Info(req.Context(), "got push event")
	body, err := github.ValidatePayload(req, p.Token)
	if err != nil {
		p.Logger.Warn(req.Context(), "unable to validate payload", zap.Error(err))
		return &pushEventResponse{
			code: http.StatusForbidden,
			msg:  strings.NewReader(fmt.Sprintf("unable to validate payload: %v", err)),
		}
	}
	var event github.PushEvent
	if err := json.Unmarshal(body, &event); err != nil {
		p.Logger.Warn(req.Context(), "unable to unpack push event body", zap.Error(err))
		return &pushEventResponse{
			code: http.StatusBadRequest,
			msg:  strings.NewReader(fmt.Sprintf("unable to unpack push event body: %v", err)),
		}
	}
	if event.Repo == nil {
		p.Logger.Warn(req.Context(), "No repository metadata set")
		return &pushEventResponse{
			code: http.StatusBadRequest,
			msg:  strings.NewReader("no repository metadata set"),
		}
	}
	if event.Repo.SSHURL == nil {
		p.Logger.Warn(req.Context(), "No repo SSH url set")
		return &pushEventResponse{
			code: http.StatusBadRequest,
			msg:  strings.NewReader("no repository SSH url set"),
		}
	}
	logger := p.Logger.With(zap.String("repo", *event.Repo.SSHURL))
	checkout, exists := p.Checkouts[*event.Repo.SSHURL]
	if !exists {
		logger.Warn(req.Context(), "cannot find checkout")
		return &pushEventResponse{
			code: http.StatusBadRequest,
			msg:  strings.NewReader("cannot find checkout"),
		}
	}
	if err := checkout.Refresh(req.Context()); err != nil {
		logger.Warn(req.Context(), "cannot refresh repository", zap.Error(err))
		return &pushEventResponse{
			code: http.StatusInternalServerError,
			msg:  strings.NewReader(fmt.Sprintf("cannot refresh repository: %v", err)),
		}
	}
	return &pushEventResponse{
		code: http.StatusOK,
		msg:  strings.NewReader(fmt.Sprintf("refreshed repository %s", *event.Repo.SSHURL)),
	}
}
