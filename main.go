package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

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
	Repos         string
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
		ListenAddr:    os.Getenv("LISTEN_ADDR"),
		DataDirectory: os.Getenv("DATA_DIRECTORY"),
		Repos:         os.Getenv("GITDB_REPOS"),
	}.WithDefaults()
}

func main() {
	instance.Main()
}

type Service struct {
	osExit   func(int)
	config   config
	log      *zap.Logger
	onListen func(net.Listener)
	server   *http.Server
}

var instance = Service{
	osExit: os.Exit,
	config: getConfig(),
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
	m.log.Info("Starting")
	rootTracer := setupTracing(m.log.With(zap.String("section", "setup_tracing")))
	co, err := setupGitServer(cfg, m.log)
	if err != nil {
		m.log.Panic("uanble to setup git server", zap.Error(err))
		m.osExit(1)
		return
	}
	m.server = setupServer(cfg, m.log, rootTracer, co)

	ln, err := net.Listen("tcp", m.server.Addr)
	if err != nil {
		m.log.Panic("unable to listen to port", zap.Error(err), zap.String("addr", m.server.Addr))
		m.osExit(1)
		return
	}
	if m.onListen != nil {
		m.onListen(ln)
	}

	serveErr := m.server.Serve(ln)
	if serveErr != http.ErrServerClosed {
		logIfErr(m.log, serveErr, "server exited")
	}
	m.log.Info("Server finished")
	if serveErr != nil {
		m.osExit(1)
	}
}

func sanitizeDir(s string) string {
	allowed := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890-"
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(allowed, r) {
			return r
		}
		return '_'
	}, s)
}

func setupGitServer(cfg config, logger *zap.Logger) (*checkoutHandler, error) {
	logger.Info("setting up git server")
	g := gitOperator{
		log: logger,
	}
	dataDir := cfg.DataDirectory
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	repos := strings.Split(cfg.Repos, ",")
	gitCheckouts := make(map[string]*gitCheckout)
	ctx := context.Background()
	for _, repo := range repos {
		cloneInto, err := ioutil.TempDir(dataDir, "gitdb_repo_"+sanitizeDir(repo))
		if err != nil {
			return nil, fmt.Errorf("unable to make temp dir for %s,%s: %v", dataDir, "gitdb_repo_"+sanitizeDir(repo), err)
		}
		co, err := g.clone(ctx, cloneInto, repo)
		if err != nil {
			return nil, fmt.Errorf("unable to clone repo %s: %v", repo, err)
		}
		gitCheckouts[getRepoKey(repo)] = co
		logger.Info("setup checkout", zap.String("repo", repo), zap.String("key", getRepoKey(repo)))
	}
	ret := &checkoutHandler{
		checkouts: gitCheckouts,
		log:       logger.With(zap.String("class", "checkout_handler")),
	}
	ret.setupMux()
	return ret, nil
}

func getRepoKey(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return repo
	}
	parts2 := strings.Split(parts[1], ".")
	if len(parts2) != 2 {
		return repo
	}
	return parts2[0]
}

func setupServer(cfg config, z *zap.Logger, rootTracer *Tracing, coHandler http.Handler) *http.Server {
	mux := rootTracer.CreateRootMux()
	mux.Handle("/health", HealthHandler(z.With(zap.String("handler", "health"))))
	mux.Handle("/", coHandler)

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
