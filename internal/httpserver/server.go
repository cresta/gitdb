package httpserver

import (
	"context"
	"crypto/rsa"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cresta/gitdb/internal/gitdb/tracing"
	"github.com/cresta/gitdb/internal/log"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

func HealthHandler(z *log.Logger, tracer tracing.Tracing) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// Note: I may need to eventually abstarct this per tracing handler
		tracer.AttachTag(req.Context(), "sampling.priority", 0)
		_, err := io.WriteString(rw, "OK")
		z.IfErr(err).Warn(req.Context(), "unable to write back health response")
	})
}

type CanHTTPWrite interface {
	HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger)
}

type BasicResponse struct {
	Code    int
	Msg     io.WriterTo
	Headers map[string]string
}

func (g *BasicResponse) HTTPWrite(ctx context.Context, w http.ResponseWriter, l *log.Logger) {
	for k, v := range g.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(g.Code)
	if w != nil {
		_, err := g.Msg.WriteTo(w)
		l.IfErr(err).Error(ctx, "unable to write final object")
	}
}

func BasicHandler(handler func(request *http.Request) CanHTTPWrite, l *log.Logger) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handler(request).HTTPWrite(request.Context(), writer, l)
	})
}

func LogMiddleware(logger *log.Logger, filterFunc func(req *http.Request) bool) func(handler http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			start := time.Now()
			defer func() {
				if !filterFunc(request) {
					logger.Info(request.Context(), "end request", zap.Duration("total_time", time.Since(start)))
				}
			}()
			handler.ServeHTTP(writer, request)
		})
	}
}

func MuxMiddleware() func(handler http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			r := mux.CurrentRoute(request)
			if r != nil {
				for k, v := range mux.Vars(request) {
					request = request.WithContext(log.With(request.Context(), zap.String(fmt.Sprintf("mux.vars.%s", k), v)))
				}
				if r.GetName() != "" {
					request = request.WithContext(log.With(request.Context(), zap.String("mux.name", r.GetName())))
				}
			}
			handler.ServeHTTP(writer, request)
		})
	}
}

func NotFoundHandler(logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		logger.With(zap.String("handler", "not_found"), zap.String("url", req.URL.String())).Warn(req.Context(), "unknown request")
		http.NotFoundHandler().ServeHTTP(rw, req)
	})
}

type JWTSignIn struct {
	Logger        *log.Logger
	Auth          func(username string, password string) (bool, error)
	SigningString func(username string) *rsa.PrivateKey
}

func (j *JWTSignIn) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	user, pass, ok := request.BasicAuth()
	if !ok {
		resp := BasicResponse{
			Code: http.StatusForbidden,
			Msg:  strings.NewReader("no basic auth information"),
		}
		resp.HTTPWrite(request.Context(), writer, j.Logger)
		return
	}
	ok, err := j.Auth(user, pass)
	if err != nil {
		resp := BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader("unable to verify auth"),
		}
		j.Logger.IfErr(err).Warn(request.Context(), "unable to auth")
		resp.HTTPWrite(request.Context(), writer, j.Logger)
		return
	}
	if !ok {
		resp := BasicResponse{
			Code: http.StatusForbidden,
			Msg:  strings.NewReader("incorrect credentials"),
		}
		j.Logger.Info(request.Context(), "bad auth", zap.String("user", user))
		resp.HTTPWrite(request.Context(), writer, j.Logger)
		return
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, &jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		Issuer:    "gitdb",
		NotBefore: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	})
	s, err := token.SignedString(j.SigningString(user))
	if err != nil {
		resp := BasicResponse{
			Code: http.StatusInternalServerError,
			Msg:  strings.NewReader("unable to sign token"),
		}
		j.Logger.IfErr(err).Warn(request.Context(), "unable to sign token")
		resp.HTTPWrite(request.Context(), writer, j.Logger)
		return
	}
	resp := BasicResponse{
		Code: http.StatusOK,
		Msg:  strings.NewReader(s),
	}
	j.Logger.Info(request.Context(), "Signed token", zap.String("user", user))
	resp.HTTPWrite(request.Context(), writer, j.Logger)
}

var _ http.Handler = &JWTSignIn{}
