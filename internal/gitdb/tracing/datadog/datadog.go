package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cresta/gitdb/internal/gitdb/tracing"
	"github.com/gorilla/mux"

	"github.com/cresta/gitdb/internal/log"

	"go.uber.org/zap"
	ddtrace2 "gopkg.in/DataDog/dd-trace-go.v1/contrib/gorilla/mux"
	ddhttp "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

// Abstract these constants out
const ddApmFile = "/var/run/datadog/apm.socket"
const ddStatsFile = "/var/run/datadog/dsd.socket"

var _ tracing.Constructor = NewTracer

type config struct {
	ApmFile   string `json:"DD_APM_RECEIVER_SOCKET"`
	StatsFile string `json:"DD_DOGSTATSD_SOCKET"`
}

func (c *config) apmFile() string {
	if c.ApmFile == "" {
		return ddApmFile
	}
	return c.ApmFile
}

func (c *config) statsFile() string {
	if c.StatsFile == "" {
		return ddStatsFile
	}
	return c.StatsFile
}

func envToStruct(env []string, into interface{}) error {
	m := make(map[string]string)
	for _, e := range env {
		p := strings.SplitN(e, "=", 1)
		if len(p) != 2 {
			continue
		}
		m[p[0]] = p[1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("unable to convert environment into map: %w", err)
	}
	return json.Unmarshal(b, into)
}

func NewTracer(originalConfig tracing.Config) (tracing.Tracing, error) {
	var cfg config
	if err := envToStruct(originalConfig.Env, &cfg); err != nil {
		return nil, fmt.Errorf("unable to convert env to config: %w", err)
	}
	if !fileExists(cfg.apmFile()) {
		originalConfig.Log.Info(context.Background(), "Unable to find datadog APM file", zap.String("file_name", cfg.apmFile()))
		return nil, nil
	}
	u := &unixRoundTripper{
		file:        cfg.apmFile(),
		dialTimeout: 100 * time.Millisecond,
	}

	startOptions := []tracer.StartOption{
		tracer.WithRuntimeMetrics(), tracer.WithHTTPRoundTripper(u), tracer.WithDogstatsdAddress("unix://" + cfg.statsFile()), tracer.WithLogger(ddZappedLogger{originalConfig.Log}),
	}
	if fileExists(cfg.statsFile()) {
		startOptions = append(startOptions, tracer.WithDogstatsdAddress("unix://"+cfg.statsFile()))
	}
	tracer.Start(startOptions...)
	originalConfig.Log.Info(context.Background(), "DataDog tracing enabled")
	return &Tracing{}, nil
}

var _ tracing.Tracing = &Tracing{}

type Tracing struct {
}

func (t *Tracing) AttachTag(ctx context.Context, key string, value interface{}) {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return
	}
	sp.SetTag(key, value)
}

func (t *Tracing) DynamicFields() []log.DynamicFields {
	return []log.DynamicFields{
		func(ctx context.Context) []zap.Field {
			sp, ok := tracer.SpanFromContext(ctx)
			if !ok || sp.Context().TraceID() == 0 {
				return nil
			}
			return []zap.Field{
				zap.Uint64("dd.trace_id", sp.Context().TraceID()),
				zap.Uint64("dd.span_id", sp.Context().SpanID()),
			}
		},
	}
}

func (t *Tracing) CreateRootMux() (*mux.Router, http.Handler) {
	ret := ddtrace2.NewRouter(ddtrace2.WithServiceName("gitdb"))
	return ret.Router, ret
}

func (t *Tracing) WrapRoundTrip(rt http.RoundTripper) http.RoundTripper {
	if t == nil {
		return rt
	}
	return ddhttp.WrapRoundTripper(rt)
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

type ddZappedLogger struct {
	*log.Logger
}

func (d ddZappedLogger) Log(msg string) {
	d.Logger.Info(context.Background(), msg)
}

type unixRoundTripper struct {
	file        string
	dialTimeout time.Duration
	transport   http.Transport
	once        sync.Once
}

func (u *unixRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u.once.Do(u.setup)
	u.transport.DialContext = u.dialContext
	u.transport.DisableCompression = true
	return u.transport.RoundTrip(req)
}

func (u *unixRoundTripper) setup() {
	u.transport.DialContext = u.dialContext
	u.transport.DisableCompression = true
}

func (u *unixRoundTripper) dialContext(ctx context.Context, _ string, _ string) (conn net.Conn, err error) {
	d := net.Dialer{
		Timeout: u.dialTimeout,
	}
	return d.DialContext(ctx, "unix", u.file)
}

var _ http.RoundTripper = &unixRoundTripper{}
