package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/cresta/gitdb/internal/gitdb/goget"

	"github.com/cresta/gitdb/internal/gitdb/tracing"

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
	Tracing   tracing.Tracing
}

func Setup(pushToken string, logger *log.Logger, handler *gitdb.CheckoutHandler, tracer tracing.Tracing) *Provider {
	if pushToken == "" {
		logger.Info(context.Background(), "no github push token.  Not setting up github push notifier")
		return nil
	}
	ret := &Provider{
		Tracing:   tracer,
		Token:     []byte(pushToken),
		Logger:    logger.With(zap.String("class", "github.Provider")),
		Checkouts: uselessCasting(handler.CheckoutsByRepo()),
	}
	return ret
}

func uselessCasting(in map[string]*goget.GitCheckout) map[string]GitCheckout {
	ret := make(map[string]GitCheckout)
	for k, v := range in {
		ret[k] = v
	}
	return ret
}

func (p *Provider) SetupMux(mux *mux.Router) {
	mux.Methods(http.MethodPost).Path("/public/github/webhook").Handler(httpserver.BasicHandler(p.githubWebhook, p.Logger)).Name("webhook")
}

func (p *Provider) pingEvent(req *http.Request, _ interface{}) httpserver.CanHTTPWrite {
	p.Logger.Info(req.Context(), "ping event")
	return &httpserver.BasicResponse{
		Code: http.StatusOK,
		Msg:  strings.NewReader("PONG"),
	}
}

func (p *Provider) pushEvent(req *http.Request, evt interface{}) httpserver.CanHTTPWrite {
	p.Logger.Info(req.Context(), "push event")
	event, ok := evt.(*github.PushEvent)
	if !ok {
		p.Logger.Error(req.Context(), "unable to cast event")
		return &httpserver.BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader("unable to cast push event"),
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

// TODO: Also log out the event type (should be in headers)
func (p *Provider) githubWebhook(req *http.Request) httpserver.CanHTTPWrite {
	hookType := github.WebHookType(req)
	if hookType == "" {
		p.Logger.Warn(req.Context(), "invalid webhook type")
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader("could not find webhook type"),
		}
	}
	p.Tracing.AttachTag(req.Context(), "github.hook_type", hookType)
	body, err := github.ValidatePayload(req, p.Token)
	if err != nil {
		p.Logger.Warn(req.Context(), "unable to validate payload", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusForbidden,
			Msg:  strings.NewReader(fmt.Sprintf("unable to validate payload: %v", err)),
		}
	}
	evt, err := github.ParseWebHook(hookType, body)
	if err != nil {
		p.Logger.Warn(req.Context(), "unable to parse webhook", zap.Error(err))
		return &httpserver.BasicResponse{
			Code: http.StatusBadRequest,
			Msg:  strings.NewReader(fmt.Sprintf("cannot parse webhook: %v", err)),
		}
	}
	eventsToProcessor := map[string]func(*http.Request, interface{}) httpserver.CanHTTPWrite{
		"ping": p.pingEvent,
		"push": p.pushEvent,
	}
	processor, exists := eventsToProcessor[hookType]
	if !exists {
		return &httpserver.BasicResponse{
			Code: http.StatusNotAcceptable,
			Msg:  strings.NewReader(fmt.Sprintf("cannot process event: %s", hookType)),
		}
	}
	return processor(req, evt)
}
