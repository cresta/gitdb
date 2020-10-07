package log

import (
	"context"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"go.uber.org/zap"
)

type loggerVal string

var loggerKey = loggerVal("key")

func With(ctx context.Context, fields ...zap.Field) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	existingFieldsVal := ctx.Value(loggerKey)
	if existingFieldsVal == nil {
		return context.WithValue(ctx, loggerKey, fields)
	}
	existingFields := existingFieldsVal.([]zap.Field)
	newFields := make([]zap.Field, 0, len(existingFields))
	newFields = append(newFields, existingFields...)
	newFields = append(newFields, fields...)
	return context.WithValue(ctx, loggerKey, newFields)
}

func fields(ctx context.Context) []zap.Field {
	existingFieldsVal := ctx.Value(loggerKey)
	if existingFieldsVal == nil {
		return nil
	}
	return existingFieldsVal.([]zap.Field)
}

func GetLogger(ctx context.Context, z *zap.Logger) *zap.Logger {
	return z.With(fields(ctx)...).With(datadogFields(ctx)...)
}

func New(root *zap.Logger) *Logger {
	return &Logger{
		root: root,
	}
}

type DynamicFields func(ctx context.Context) []zap.Field

type Logger struct {
	root          *zap.Logger
	dynamicFields []DynamicFields
}

func (l *Logger) Info(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.root.WithOptions(zap.Hooks())
	GetLogger(ctx, l.root).WithOptions(zap.AddCallerSkip(1)).Info(msg, fields...)
}

func (l *Logger) Warn(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(ctx, l.root).WithOptions(zap.AddCallerSkip(1)).Warn(msg, fields...)
}

func (l *Logger) Error(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(ctx, l.root).WithOptions(zap.AddCallerSkip(1)).Error(msg, fields...)
}

func (l *Logger) Panic(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(ctx, l.root).WithOptions(zap.AddCallerSkip(1)).Panic(msg, fields...)
}

func (l *Logger) IfErr(err error) *Logger {
	if err == nil {
		return nil
	}
	return l.With(zap.Error(err))
}

func (l *Logger) DynamicFields(dynamicFields ...DynamicFields) *Logger {
	ret := &Logger{
		root:          l.root,
		dynamicFields: make([]DynamicFields, 0, len(dynamicFields)+len(l.dynamicFields)),
	}
	ret.dynamicFields = append(ret.dynamicFields, l.dynamicFields...)
	ret.dynamicFields = append(ret.dynamicFields, dynamicFields...)
	return ret
}

func (l *Logger) With(fields ...zap.Field) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{
		root: l.root.With(fields...),
	}
}

// TODO: Move this out of this file
func datadogFields(ctx context.Context) []zap.Field {
	sp, ok := tracer.SpanFromContext(ctx)
	if !ok || sp.Context().TraceID() == 0 {
		return nil
	}
	return []zap.Field{
		zap.Uint64("dd.trace_id", sp.Context().TraceID()),
		zap.Uint64("dd.span_id", sp.Context().SpanID()),
	}
}
