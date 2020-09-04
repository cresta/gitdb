package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"go.uber.org/zap"
	ddhttp "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http"
)

func setupLogging() *zap.Logger {
	l, err := zap.NewProduction()
	if err != nil {
		log.Println("Unable to setup zap logger")
		panic(err)
	}
	return l
}

type ContextZapLogger struct {
	logger *zap.Logger
}

func (c *ContextZapLogger) With(ctx context.Context) *zap.Logger {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return c.logger
	}
	return c.logger.With(zap.Uint64("dd.trace_id", sp.Context().TraceID()), zap.Uint64("dd.span_id", sp.Context().SpanID()))
}

type Tracing struct {
}

func (t *Tracing) WrapRoundTrip(rt http.RoundTripper) http.RoundTripper {
	if t == nil {
		return rt
	}
	return ddhttp.WrapRoundTripper(rt)
}

type CoreMux interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

var _ CoreMux = http.NewServeMux()

func (t *Tracing) CreateRootMux() CoreMux {
	if t == nil {
		return http.NewServeMux()
	}
	return ddhttp.NewServeMux(ddhttp.WithServiceName("reviewbot"))
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

const ddApmFile = "/var/run/datadog/apm.socket"
const ddStatsFile = "/var/run/datadog/dsd.socket"

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

type ddZappedLogger struct {
	*zap.Logger
}

func (d ddZappedLogger) Log(msg string) {
	d.Logger.Info(msg)
}

var _ http.RoundTripper = &unixRoundTripper{}

func setupTracing(logger *zap.Logger) *Tracing {
	if !fileExists(ddApmFile) {
		logger.Info("Unable to find datadog APM file", zap.String("file_name", ddApmFile))
		return nil
	}
	u := &unixRoundTripper{
		file:        ddApmFile,
		dialTimeout: 100 * time.Millisecond,
	}

	tracer.Start(tracer.WithRuntimeMetrics(), tracer.WithHTTPRoundTripper(u), tracer.WithDogstatsdAddress("unix://"+ddStatsFile), tracer.WithLogger(ddZappedLogger{logger}))
	logger.Info("DataDog tracing enabled")
	return &Tracing{}
}

type config struct {
	ListenAddr string
}

func getConfig() config {
	return config{
		// Defaults to ":8080"
		ListenAddr: os.Getenv("LISTEN_ADDR"),
	}
}

func main() {
	cfg := getConfig()
	zapLogger := setupLogging()
	zapLogger.Info("Starting")
	rootTracer := setupTracing(zapLogger.With(zap.String("section", "setup_tracing")))
	ss := setupServer(cfg, zapLogger, rootTracer)
	listenErr := ss.ListenAndServe()
	logIfErr(zapLogger, listenErr, "server exited")
	zapLogger.Info("Server finished")
	if listenErr != nil {
		os.Exit(1)
	}
}

func setupServer(cfg config, z *zap.Logger, rootTracer *Tracing) *http.Server {
	mux := rootTracer.CreateRootMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		(&ContextZapLogger{z}).With(req.Context()).With(zap.String("handler", "not_found"), zap.String("url", req.URL.String())).Warn("unknown request")
		http.NotFoundHandler().ServeHTTP(rw, req)
	})
	mux.Handle("/health", HealthHandler(z.With(zap.String("handler", "health"))))
	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	return &http.Server{
		Handler: mux,
		Addr:    listenAddr,
	}
}

func HealthHandler(z *zap.Logger) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		attachTag(req.Context(), "sampling.priority", 0)
		_, err := io.WriteString(rw, "OK")
		logIfErr(z, err, "unable to write back health response")
	})
}

func logIfErr(logger *zap.Logger, err error, s string) {
	if err != nil {
		logger.Error(s, zap.Error(err))
	}
}
func attachTag(ctx context.Context, key string, value interface{}) {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return
	}
	sp.SetTag(key, value)
}
