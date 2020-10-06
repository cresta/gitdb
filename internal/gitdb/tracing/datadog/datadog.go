package datadog

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cresta/gitdb/internal/log"

	"github.com/gorilla/mux"

	"go.uber.org/zap"
	ddhttp "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

const ddApmFile = "/var/run/datadog/apm.socket"
const ddStatsFile = "/var/run/datadog/dsd.socket"

func NewTracer(logger *log.Logger) *Tracing {
	if !fileExists(ddApmFile) {
		logger.Info(context.Background(), "Unable to find datadog APM file", zap.String("file_name", ddApmFile))
		return nil
	}
	u := &unixRoundTripper{
		file:        ddApmFile,
		dialTimeout: 100 * time.Millisecond,
	}

	tracer.Start(tracer.WithRuntimeMetrics(), tracer.WithHTTPRoundTripper(u), tracer.WithDogstatsdAddress("unix://"+ddStatsFile), tracer.WithLogger(ddZappedLogger{logger}))
	logger.Info(context.Background(), "DataDog tracing enabled")
	return &Tracing{}
}

type Tracing struct {
}

func (t *Tracing) WrapRoundTrip(rt http.RoundTripper) http.RoundTripper {
	if t == nil {
		return rt
	}
	return ddhttp.WrapRoundTripper(rt)
}

func (t *Tracing) CreateRootMux() *mux.Router {
	if t == nil {
		return mux.NewRouter()
	}
	wrapped := ddhttp.NewServeMux(ddhttp.WithServiceName("gitdb"))

	retMux := mux.NewRouter()
	retMux.Use(func(abc http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			a, _ := wrapped.Handler(request)
			a.ServeHTTP(writer, request)
		})
	})
	return retMux
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
