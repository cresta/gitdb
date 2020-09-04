package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"github.com/google/go-github/v29/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
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
	GithubToken string
	SlackSecret string
	ListenAddr  string
}

func getConfig() config {
	return config{
		GithubToken: os.Getenv("GITHUB_TOKEN"),
		SlackSecret: os.Getenv("SLACK_SECRET"),
		// Defaults to ":8080"
		ListenAddr: os.Getenv("LISTEN_ADDR"),
	}
}

func main() {
	cfg := getConfig()
	zapLogger := setupLogging()
	zapLogger.Info("Starting")
	rootTracer := setupTracing(zapLogger.With(zap.String("section", "setup_tracing")))
	rb, err := newReviewBot(cfg.GithubToken, zapLogger, rootTracer)
	if err != nil {
		fmt.Println("Unable to make basic review bot")
		os.Exit(1)
		return
	}
	ss := setupServer(cfg, zapLogger, rb, rootTracer)
	listenErr := ss.ListenAndServe()
	logIfErr(zapLogger, listenErr, "server exited")
	zapLogger.Info("Server finished")
	if listenErr != nil {
		os.Exit(1)
	}
}

type Reviewbot struct {
	authUser       *github.User
	client         *github.Client
	logger         ContextZapLogger
	defaultTimeout time.Duration
}

func newReviewBot(token string, zapLogger *zap.Logger, rootTracer *Tracing) (*Reviewbot, error) {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	tc.Transport = rootTracer.WrapRoundTrip(tc.Transport)
	client := github.NewClient(tc)
	authUser, _, err := client.Users.Get(ctx, "")
	if err != nil {
		zapLogger.Error("unable to get an authenticated user", zap.Error(err))
		return nil, err
	}
	return &Reviewbot{
		authUser:       authUser,
		client:         client,
		logger:         ContextZapLogger{zapLogger},
		defaultTimeout: time.Second * 5,
	}, nil
}

func (r *Reviewbot) acceptReview(ctx context.Context, owner string, repo string, prNumber int, acceptMsg string) error {
	funcLogger := r.logger.With(ctx).With(zap.String("owner", owner), zap.String("repo", repo), zap.Int("pr", prNumber))
	funcLogger.Info("Asked to accept review")
	if r.defaultTimeout != 0 {
		var onEnd func()
		ctx, onEnd = context.WithTimeout(ctx, r.defaultTimeout)
		defer onEnd()
	}
	pr, _, err := r.client.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		funcLogger.Error("unable to get pull requests", zap.Error(err))
		return err
	}
	if !isRequestedReviewer(r.authUser, pr.RequestedReviewers) {
		funcLogger.Warn("expected to accept, but not a reviewer")
		return errors.New("not a reviewer")
	}
	_, _, err = r.client.PullRequests.CreateReview(ctx, owner, repo, prNumber, &github.PullRequestReviewRequest{
		Body:  &acceptMsg,
		Event: github.String("APPROVE"),
	})
	if err != nil {
		funcLogger.Error("unable to accept review", zap.Error(err))
		return err
	}
	funcLogger.Info("Accepted review")
	return nil
}

func isRequestedReviewer(exp *github.User, users []*github.User) bool {
	for _, u := range users {
		if *u.ID == *exp.ID {
			return true
		}
	}
	return false
}

func setupServer(cfg config, z *zap.Logger, rb *Reviewbot, rootTracer *Tracing) *http.Server {
	ar := &acceptReviewHandler{
		rb:     rb,
		logger: ContextZapLogger{z.With(zap.String("handler", "accept_review"))},
	}
	sv := &slackVerifier{
		expectedToken: cfg.SlackSecret,
		logger:        ContextZapLogger{z.With(zap.String("handler", "slack_verifier"))},
	}
	ss := slackServer{
		handlers: map[string]SlashHandler{
			"accept": ar,
		},
		logger: ContextZapLogger{z.With(zap.String("handler", "slack_server"))},
	}
	mux := rootTracer.CreateRootMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		(&ContextZapLogger{z}).With(req.Context()).With(zap.String("handler", "not_found"), zap.String("url", req.URL.String())).Warn("unknown request")
		http.NotFoundHandler().ServeHTTP(rw, req)
	})
	mux.Handle("/health", HealthHandler(z.With(zap.String("handler", "health"))))
	mux.Handle("/slack", sv.WithHandler(&ss))
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
