package main

import (
	"context"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	"io"
	"net/http"
	"os"

	"go.uber.org/zap"
)

type CoreMux interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

var _ CoreMux = http.NewServeMux()

type config struct {
	ListenAddr    string
	DataDirectory string
}

func (c config) WithDefaults() config {
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.DataDirectory == "" {
		c.DataDirectory = "/tmp"
	}
	return c
}

func getConfig() config {
	return config{
		// Defaults to ":8080"
		ListenAddr:    os.Getenv("LISTEN_ADDR"),
		DataDirectory: os.Getenv("DATA_DIRECTORY"),
	}.WithDefaults()
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
	return &http.Server{
		Handler: mux,
		Addr:    cfg.ListenAddr,
	}
}

func HealthHandler(z *zap.Logger) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		attachTag(req.Context(), "sampling.priority", 0)
		_, err := io.WriteString(rw, "OK")
		logIfErr(z, err, "unable to write back health response")
	})
}

func attachTag(ctx context.Context, key string, value interface{}) {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return
	}
	sp.SetTag(key, value)
}
