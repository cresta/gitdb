package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cresta/gitdb/internal/gitdb"
	"github.com/cresta/gitdb/internal/httpserver"

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

func Setup(pushToken string, logger *log.Logger, handler *gitdb.CheckoutHandler) *Provider {
	if pushToken == "" {
		logger.Info(context.Background(), "no github push token.  Not setting up github push notifier")
		return nil
	}
	ret := &Provider{
		Token:     []byte(pushToken),
		Logger:    logger.With(zap.String("class", "github.Provider")),
		Checkouts: uselessCasting(handler.CheckoutsByRepo()),
	}
	return ret
}

func uselessCasting(in map[string]*gitdb.GitCheckout) map[string]GitCheckout {
	ret := make(map[string]GitCheckout)
	for k, v := range in {
		ret[k] = v
	}
	return ret
}

func (p *Provider) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodPost).Path("/public/github/push_event").Handler(httpserver.BasicHandler(p.pushEvent, p.Logger)).Name("push_event")
}

// TODO: Also log out the event type (should be in headers)
func (p *Provider) pushEvent(req *http.Request) httpserver.CanHTTPWrite {
	p.Logger.Info(req.Context(), "got push event")
	body, err := github.ValidatePayload(req, p.Token)
	if err != nil {
		p.Logger.Warn(req.Context(), "unable to validate payload", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusForbidden,
			Msg:  strings.NewReader(fmt.Sprintf("unable to validate payload: %v", err)),
		}
	}
	var event github.PushEvent
	if err := json.Unmarshal(body, &event); err != nil {
		p.Logger.Warn(req.Context(), "unable to unpack push event body", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader(fmt.Sprintf("unable to unpack push event body: %v", err)),
		}
	}
	if event.Repo == nil {
		p.Logger.Warn(req.Context(), "No repository metadata set")
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader("no repository metadata set"),
		}
	}
	if event.Repo.SSHURL == nil {
		p.Logger.Warn(req.Context(), "No repo SSH url set")
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader("no repository SSH url set"),
		}
	}
	logger := p.Logger.With(zap.String("repo", *event.Repo.SSHURL))
	checkout, exists := p.Checkouts[*event.Repo.SSHURL]
	if !exists {
		logger.Warn(req.Context(), "cannot find checkout")
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader("cannot find checkout"),
		}
	}
	if err := checkout.Refresh(req.Context()); err != nil {
		logger.Warn(req.Context(), "cannot refresh repository", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader(fmt.Sprintf("cannot refresh repository: %v", err)),
		}
	}
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  strings.NewReader(fmt.Sprintf("refreshed repository %s", *event.Repo.SSHURL)),
	}
}
