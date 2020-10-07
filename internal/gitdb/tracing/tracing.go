package tracing

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cresta/gitdb/internal/log"
	"github.com/gorilla/mux"
)

type Tracing interface {
	WrapRoundTrip(rt http.RoundTripper) http.RoundTripper
	AttachTag(ctx context.Context, key string, value interface{})
	DynamicFields() []log.DynamicFields
	CreateRootMux() (*mux.Router, http.Handler)
}

type Constructor func(config Config) (Tracing, error)

type Registry struct {
	Constructors map[string]Constructor
}

func (r *Registry) New(name string, config Config) (Tracing, error) {
	if name == "" || r == nil {
		config.Log.Info(context.Background(), "returning no-op tracer")
		return Noop{}, nil
	}
	cons, exists := r.Constructors[name]
	if !exists {
		return nil, fmt.Errorf("unable to find tracer named: %s", name)
	}
	ret, err := cons(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create registry %s: %w", name, err)
	}
	return ret, nil
}

type Config struct {
	Log *log.Logger
	Env []string
}

var _ Tracing = Noop{}

type Noop struct{}

func (n Noop) WrapRoundTrip(rt http.RoundTripper) http.RoundTripper {
	return rt
}

func (n Noop) AttachTag(ctx context.Context, key string, value interface{}) {
}

func (n Noop) DynamicFields() []log.DynamicFields {
	return nil
}

func (n Noop) CreateRootMux() (*mux.Router, http.Handler) {
	ret := mux.NewRouter()
	return ret, ret
}
