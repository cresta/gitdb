package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/dgrijalva/jwt-go"

	"github.com/cresta/gitdb/internal/gitdb/tracing/datadog"
	"github.com/signalfx/golib/v3/httpdebug"

	"github.com/cresta/gitdb/internal/gitdb/tracing"
	"github.com/cresta/gitdb/internal/httpserver"
	"github.com/cresta/gitdb/internal/log"

	"github.com/cresta/gitdb/internal/gitdb/repoprovider/github"

	"github.com/cresta/gitdb/internal/gitdb"
	"go.uber.org/zap"
)

type config struct {
	ListenAddr        string
	DataDirectory     string
	DebugListenAddr   string
	Repos             string
	PrivateKey        string
	PrivateKeyPasswd  string
	GithubPushToken   string
	Tracer            string
	JWTSecret         string
	JWTSignInUsername string
}

func (c config) WithDefaults() config {
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.DataDirectory == "" {
		c.DataDirectory = os.TempDir()
	}
	if c.DebugListenAddr == "" {
		c.DebugListenAddr = ":6060"
	}
	return c
}

func getConfig() config {
	return config{
		// Defaults to ":8080"
		ListenAddr:      os.Getenv("LISTEN_ADDR"),
		DataDirectory:   os.Getenv("DATA_DIRECTORY"),
		Repos:           os.Getenv("GITDB_REPOS"),
		PrivateKey:      os.Getenv("GITDB_PRIVATE_KEY"),
		GithubPushToken: os.Getenv("GITHUB_PUSH_TOKEN"),
		// Defaults to ":6060"
		DebugListenAddr:   os.Getenv("GITDB_DEBUG_ADDR"),
		PrivateKeyPasswd:  os.Getenv("GITDB_PRIVATE_KEY_PASSWD"),
		Tracer:            os.Getenv("GITDB_TRACER"),
		JWTSecret:         os.Getenv("GITDB_JWT_SECRET"),
		JWTSignInUsername: os.Getenv("GITDB_JWT_SIGNIN_USERNAME"),
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
	tracers: &tracing.Registry{
		Constructors: map[string]tracing.Constructor{
			"datadog": datadog.NewTracer,
		},
	},
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
	gitdb.WrapGitProtocols(rootTracer)
	m.log = m.log.DynamicFields(rootTracer.DynamicFields()...)

	co, err := gitdb.NewHandler(m.log, gitdb.Config{
		DataDirectory:    cfg.DataDirectory,
		Repos:            cfg.Repos,
		PrivateKey:       cfg.PrivateKey,
		PrivateKeyPasswd: cfg.PrivateKeyPasswd,
	}, rootTracer)
	if err != nil {
		m.log.IfErr(err).Panic(context.Background(), "unable to setup git server")
		m.osExit(1)
		return
	}
	githubListener := github.Setup(cfg.GithubPushToken, m.log, co, rootTracer)
	m.server = setupServer(cfg, m.log, rootTracer, co, githubListener)
	shutdownCallback, err := setupDebugServer(m.log, cfg.DebugListenAddr, m)
	if err != nil {
		m.log.IfErr(err).Panic(context.Background(), "unable to setup debug server")
		m.osExit(1)
		return
	}

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
	shutdownCallback()
	if serveErr != nil {
		m.osExit(1)
	}
}

func setupDebugServer(l *log.Logger, listenAddr string, obj interface{}) (func(), error) {
	if listenAddr == "" || listenAddr == "-" {
		return func() {
		}, nil
	}
	ret := httpdebug.New(&httpdebug.Config{
		Logger:        &log.FieldLogger{Logger: l},
		ExplorableObj: obj,
	})
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("unable to listen to %s: %w", listenAddr, err)
	}
	go func() {
		serveErr := ret.Server.Serve(ln)
		if serveErr != http.ErrServerClosed {
			l.IfErr(serveErr).Error(context.Background(), "debug server existed")
		}
		l.Info(context.Background(), "debug server finished")
	}()
	return func() {
		err := ln.Close()
		l.IfErr(err).Warn(context.Background(), "unable to close listening socket for debug server")
	}, nil
}

func setupServer(cfg config, z *log.Logger, rootTracer tracing.Tracing, coHandler *gitdb.CheckoutHandler, githubProvider *github.Provider) *http.Server {
	rootMux, rootHandler := rootTracer.CreateRootMux()
	rootMux.Use(httpserver.MuxMiddleware())
	rootMux.Use(httpserver.LogMiddleware(z, func(req *http.Request) bool {
		return req.URL.Path == "/health"
	}))
	rootMux.Handle("/health", httpserver.HealthHandler(z.With(zap.String("handler", "health")), rootTracer)).Name("health")
	if githubProvider != nil {
		z.Info(context.Background(), "setting up github provider path")
		githubProvider.SetupMux(rootMux)
	}
	var keyFunc jwt.Keyfunc
	if cfg.JWTSecret != "" {
		keyFunc = func(token *jwt.Token) (interface{}, error) {
			return []byte(cfg.JWTSecret), nil
		}
		z.Info(context.Background(), "set up JWT secret")
	} else {
		z.Info(context.Background(), "skipping JWT secret setup")
	}
	if cfg.JWTSignInUsername != "" && cfg.JWTSecret != "" {
		signIn := &httpserver.JWTSignIn{
			Logger: z.With(zap.String("handler", "jwt_sign_in")),
			Auth: func(username string, password string) (bool, error) {
				return username == cfg.JWTSignInUsername && password == cfg.JWTSecret, nil
			},
			SigningString: func(username string) string {
				return cfg.JWTSecret
			},
		}
		rootMux.Handle("/signin", signIn).Name("signin")
	}
	coHandler.SetupMux(rootMux, keyFunc)
	rootMux.NotFoundHandler = httpserver.NotFoundHandler(z)
	rootMux.Use(tracing.MuxTagging(rootTracer))
	return &http.Server{
		Handler: rootHandler,
		Addr:    cfg.ListenAddr,
	}
}
