package main

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"

	"github.com/gorilla/mux"

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
	ListenAddr          string
	DataDirectory       string
	DebugListenAddr     string
	GithubPushToken     string
	RepoConfig          string
	Tracer              string
	JWTPrivateKey       string
	JWTPrivateKeyPasswd string
	JWTPublicKey        string
	JWTSignInUsername   string
	JWTSignInPassword   string
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
		ListenAddr:    os.Getenv("LISTEN_ADDR"),
		DataDirectory: os.Getenv("DATA_DIRECTORY"),
		// Defaults to ":6060"
		DebugListenAddr: os.Getenv("GITDB_DEBUG_ADDR"),
		Tracer:          os.Getenv("GITDB_TRACER"),
		RepoConfig:      os.Getenv("GITDB_REPO_CONFIG"),

		GithubPushToken:     os.Getenv("GITHUB_PUSH_TOKEN"),
		JWTPrivateKey:       os.Getenv("GITDB_JWT_PRIVATE_KEY"),
		JWTPrivateKeyPasswd: os.Getenv("GITDB_JWT_PRIVATE_KEY_PASSWD"),
		JWTPublicKey:        os.Getenv("GITDB_JWT_PUBLIC_KEY"),
		JWTSignInUsername:   os.Getenv("GITDB_JWT_SIGNIN_USERNAME"),
		JWTSignInPassword:   os.Getenv("GITDB_JWT_SIGNIN_PASSWORD"),
	}.WithDefaults()
}

type RepoConfig struct {
	Repositories []Repository
}

type Repository = gitdb.Repository

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

func (m *Service) loadRepoConfig(cfg config) (RepoConfig, error) {
	if cfg.RepoConfig == "" {
		return RepoConfig{}, nil
	}
	b, err := ioutil.ReadFile(cfg.RepoConfig)
	if err != nil {
		return RepoConfig{}, fmt.Errorf("unable to read file %s: %w", cfg.RepoConfig, err)
	}
	var ret RepoConfig
	if err := json.Unmarshal(b, &ret); err != nil {
		return RepoConfig{}, fmt.Errorf("unable to json unmarshal content of %s: %w", cfg.RepoConfig, err)
	}
	return ret, nil
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

	repoConfig, err := m.loadRepoConfig(cfg)
	if err != nil {
		m.log.IfErr(err).Error(context.Background(), "unable to load repository config")
		m.osExit(1)
		return
	}

	gitdb.WrapGitProtocols(rootTracer)
	m.log = m.log.DynamicFields(rootTracer.DynamicFields()...)

	co, err := gitdb.NewHandler(m.log, gitdb.Config{
		DataDirectory: cfg.DataDirectory,
		Repos:         repoConfig.Repositories,
	}, rootTracer)
	if err != nil {
		m.log.IfErr(err).Panic(context.Background(), "unable to setup git server")
		m.osExit(1)
		return
	}
	githubListener := github.Setup(cfg.GithubPushToken, m.log, co, rootTracer)
	m.server = setupServer(cfg, m.log, rootTracer, co, githubListener, repoConfig)
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

func setupJWT(cfg config, m *mux.Router, h *gitdb.CheckoutHandler, logger *log.Logger, repoConfig RepoConfig) error {
	if cfg.JWTPublicKey == "" {
		logger.Info(context.Background(), "skipping public JWT handler: no public key")
		return nil
	}
	fileContent, err := ioutil.ReadFile(cfg.JWTPublicKey)
	if err != nil {
		return fmt.Errorf("unable to read jwt file %s: %w", cfg.JWTPublicKey, err)
	}
	parsedPublicKey, err := jwt.ParseRSAPublicKeyFromPEM(fileContent)
	if err != nil {
		return fmt.Errorf("unable to parse public key in file %s: %w", cfg.JWTPublicKey, err)
	}
	keyFunc := func(token *jwt.Token) (interface{}, error) {
		return parsedPublicKey, nil
	}
	h.SetupPublicJWTHandler(m, keyFunc, repoConfig.Repositories)
	return nil
}

func setupJWTSigning(ctx context.Context, cfg config, log *log.Logger, m *mux.Router) error {
	if cfg.JWTSignInUsername == "" {
		log.Info(ctx, "no username set, skipping JWT signing step")
		return nil
	}
	if cfg.JWTSignInPassword == "" {
		log.Info(ctx, "no password set, skipping JWT signing step")
		return nil
	}
	if cfg.JWTPrivateKey == "" {
		log.Info(ctx, "no private key set.  Skipping JWT signing step")
		return nil
	}
	fileContent, err := ioutil.ReadFile(cfg.JWTPrivateKey)
	if err != nil {
		return fmt.Errorf("unable to read private key file %s: %w", cfg.JWTPrivateKey, err)
	}
	var pKey *rsa.PrivateKey
	var parseErr error
	if cfg.JWTPrivateKeyPasswd == "" {
		log.Info(ctx, "JWT private key password not set")
		pKey, parseErr = jwt.ParseRSAPrivateKeyFromPEM(fileContent)
		if parseErr != nil {
			return fmt.Errorf("unable to parse private key from PEM: %w", err)
		}
	} else {
		pKey, parseErr = jwt.ParseRSAPrivateKeyFromPEMWithPassword(fileContent, cfg.JWTPrivateKeyPasswd)
		if parseErr != nil {
			return fmt.Errorf("unable to parse private key from PEM: %w", err)
		}
	}
	signIn := &httpserver.JWTSignIn{
		Logger: log.With(zap.String("handler", "jwt_sign_in")),
		Auth: func(username string, password string) (bool, error) {
			return username == cfg.JWTSignInUsername && password == cfg.JWTSignInPassword, nil
		},
		SigningString: func(username string) *rsa.PrivateKey {
			return pKey
		},
	}
	m.Handle("/public/signin", signIn).Methods(http.MethodPost).Name("signin")
	return nil
}

func setupServer(cfg config, z *log.Logger, rootTracer tracing.Tracing, coHandler *gitdb.CheckoutHandler, githubProvider *github.Provider, repoConfig RepoConfig) *http.Server {
	rootMux, rootHandler := rootTracer.CreateRootMux()
	rootMux.Use(httpserver.MuxMiddleware())
	rootMux.Use(httpserver.LogMiddleware(z, func(req *http.Request) bool {
		return req.URL.Path == "/health"
	}))
	rootMux.Handle("/health", httpserver.HealthHandler(z.With(zap.String("handler", "health")), rootTracer)).Name("health")
	coHandler.SetupMux(rootMux)
	if githubProvider != nil {
		z.Info(context.Background(), "setting up github provider path")
		githubProvider.SetupMux(rootMux)
	}
	z.IfErr(setupJWT(cfg, rootMux, coHandler, z, repoConfig)).Panic(context.Background(), "unable to public JWT endpoint")
	z.IfErr(setupJWTSigning(context.Background(), cfg, z, rootMux)).Panic(context.Background(), "unable to setup JWT signing")
	rootMux.NotFoundHandler = httpserver.NotFoundHandler(z)
	rootMux.Use(tracing.MuxTagging(rootTracer))
	return &http.Server{
		Handler: rootHandler,
		Addr:    cfg.ListenAddr,
	}
}
