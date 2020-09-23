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

	"github.com/cresta/gitdb/internal/gitdb"
	"github.com/cresta/gitdb/internal/gitdb/tracing/datadog"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"go.uber.org/zap"
)

type config struct {
	ListenAddr       string
	DataDirectory    string
	Repos            string
	PrivateKey       string
	PrivateKeyPasswd string
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
		PrivateKeyPasswd: os.Getenv("GITDB_PRIVATE_KEY_PASSWD"),
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

func setupLogging() (*zap.Logger, error) {
	l, err := zap.NewProduction()
	if err != nil {
		return nil, err
	}
	return l, nil
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
	rootTracer := datadog.NewTracer(m.log.With(zap.String("section", "setup_tracing")))
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
		gitdb.LogIfErr(m.log, serveErr, "server exited")
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

func setupGitServer(cfg config, logger *zap.Logger) (*gitdb.CheckoutHandler, error) {
	logger.Info("setting up git server")
	publicKeys, err := getPrivateKey(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to load private key: %v", err)
	}
	g := gitdb.GitOperator{
		Log:  logger,
		Auth: publicKeys,
	}
	dataDir := cfg.DataDirectory
	if dataDir == "" {
		dataDir = os.TempDir()
	}
	repos := strings.Split(cfg.Repos, ",")
	gitCheckouts := make(map[string]*gitdb.GitCheckout)
	ctx := context.Background()
	for _, repo := range repos {
		cloneInto, err := ioutil.TempDir(dataDir, "gitdb_repo_"+sanitizeDir(repo))
		if err != nil {
			return nil, fmt.Errorf("unable to make temp dir for %s,%s: %v", dataDir, "gitdb_repo_"+sanitizeDir(repo), err)
		}
		co, err := g.Clone(ctx, cloneInto, repo)
		if err != nil {
			return nil, fmt.Errorf("unable to clone repo %s: %v", repo, err)
		}
		gitCheckouts[getRepoKey(repo)] = co
		logger.Info("setup checkout", zap.String("repo", repo), zap.String("key", getRepoKey(repo)))
	}
	ret := &gitdb.CheckoutHandler{
		Checkouts: gitCheckouts,
		Log:       logger.With(zap.String("class", "checkout_handler")),
	}
	ret.SetupMux()
	return ret, nil
}

func getPrivateKey(cfg config) (transport.AuthMethod, error) {
	if cfg.PrivateKey == "" {
		return nil, nil
	}
	sshKey, err := ioutil.ReadFile(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("unable to read file %s: %v", cfg.PrivateKey, err)
	}
	publicKey, err := ssh.NewPublicKeys("git", sshKey, cfg.PrivateKeyPasswd)
	if err != nil {
		return nil, fmt.Errorf("unable to load public keys: %v", err)
	}
	return publicKey, nil
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

func setupServer(cfg config, z *zap.Logger, rootTracer *datadog.Tracing, coHandler http.Handler) *http.Server {
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
		gitdb.LogIfErr(z, err, "unable to write back health response")
	})
}

func attachTag(ctx context.Context, key string, value interface{}) {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return
	}
	sp.SetTag(key, value)
}