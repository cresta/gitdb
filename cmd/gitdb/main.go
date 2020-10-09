package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/cresta/gitdb/internal/gitdb/tracing"
	"github.com/cresta/gitdb/internal/httpserver"
	"github.com/cresta/gitdb/internal/log"

	"github.com/cresta/gitdb/internal/gitdb/repoprovider/github"

	"github.com/cresta/gitdb/internal/gitdb"
	"go.uber.org/zap"
)

type config struct {
	ListenAddr       string
	DataDirectory    string
	Repos            string
	PrivateKey       string
	PrivateKeyPasswd string
	GithubPushToken  string
	Tracer           string
}

func (c config) WithDefaults() config {
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.DataDirectory == "" {
		c.DataDirectory = os.TempDir()
	}
	return c
}

func getConfig() config {
	return config{
		// Defaults to ":8080"
		ListenAddr:       os.Getenv("LISTEN_ADDR"),
		DataDirectory:    os.Getenv("DATA_DIRECTORY"),
		Repos:            os.Getenv("GITDB_REPOS"),
		PrivateKey:       os.Getenv("GITDB_PRIVATE_KEY"),
		GithubPushToken:  os.Getenv("GITHUB_PUSH_TOKEN"),
		PrivateKeyPasswd: os.Getenv("GITDB_PRIVATE_KEY_PASSWD"),
		Tracer:           os.Getenv("GITDB_TRACER"),
	}.WithDefaults()
}

func main() {
	instance.Main()
}

type Service struct {
	osExit   func(int)
	config   config
	log      *log.Logger
	onListen func(net.Listener)
	server   *http.Server
	tracers  *tracing.Registry
}

var instance = Service{
	osExit: os.Exit,
	config: getConfig(),
}

func setupLogging() (*log.Logger, error) {
	l, err := zap.NewProduction()
	if err != nil {
		return nil, err
	}
	return log.New(l), nil
}

func (m *Service) Main() {
	cfg := m.config
	if m.log == nil {
		var err error
		m.log, err = setupLogging()
		if err != nil {
			fmt.Printf("Unable to setup logging: %v", err)
			m.osExit(1)
			return
		}
	}
	m.log.Info(context.Background(), "Starting")
	rootTracer, err := m.tracers.New(m.config.Tracer, tracing.Config{
		Log: m.log.With(zap.String("section", "setup_tracing")),
		Env: os.Environ(),
	})
	if err != nil {
		m.log.IfErr(err).Error(context.Background(), "unable to setup tracing")
		m.osExit(1)
		return
	}
	m.log = m.log.DynamicFields(rootTracer.DynamicFields()...)

	co, err := gitdb.NewHandler(m.log, gitdb.Config{
		DataDirectory:    cfg.DataDirectory,
		Repos:            cfg.Repos,
		PrivateKey:       cfg.PrivateKey,
		PrivateKeyPasswd: cfg.PrivateKeyPasswd,
	})
	if err != nil {
		m.log.IfErr(err).Panic(context.Background(), "unable to setup git server")
		m.osExit(1)
		return
	}
	githubListener := github.Setup(cfg.GithubPushToken, m.log, co, rootTracer)
	m.server = setupServer(cfg, m.log, rootTracer, co, githubListener)

	ln, err := net.Listen("tcp", m.server.Addr)
	if err != nil {
		m.log.Panic(context.Background(), "unable to listen to port", zap.Error(err), zap.String("addr", m.server.Addr))
		m.osExit(1)
		return
	}
	if m.onListen != nil {
		m.onListen(ln)
	}

	serveErr := m.server.Serve(ln)
	if serveErr != http.ErrServerClosed {
		m.log.IfErr(serveErr).Error(context.Background(), "server existed")
	}
	m.log.Info(context.Background(), "Server finished")
	if serveErr != nil {
		m.osExit(1)
	}
}

func setupServer(cfg config, z *log.Logger, rootTracer tracing.Tracing, coHandler *gitdb.CheckoutHandler, githubProvider *github.Provider) *http.Server {
	rootMux, rootHandler := rootTracer.CreateRootMux()
	rootMux.Use(httpserver.MuxMiddleware())
	rootMux.Use(httpserver.LogMiddleware(z))

	rootMux.Handle("/health", httpserver.HealthHandler(z.With(zap.String("handler", "health")), rootTracer)).Name("health")
	if githubProvider != nil {
		z.Info(context.Background(), "setting up github provider path")
		githubProvider.SetupMux(rootMux)
	}
	coHandler.SetupMux(rootMux)
	rootMux.NotFoundHandler = httpserver.NotFoundHandler(z)
	return &http.Server{
		Handler: rootHandler,
		Addr:    cfg.ListenAddr,
	}
}
