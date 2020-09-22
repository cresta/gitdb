package gitdb

import (
	"context"

	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)

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

func LogIfErr(logger *zap.Logger, err error, s string) {
	if err != nil {
		logger.Error(s, zap.Error(err))
	}
}
