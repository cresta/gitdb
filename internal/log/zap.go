package log

import (
	"context"

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

func Fields(ctx context.Context) []zap.Field {
	existingFieldsVal := ctx.Value(loggerKey)
	if existingFieldsVal == nil {
		return nil
	}
	return existingFieldsVal.([]zap.Field)
}

func GetLogger(z *zap.Logger, ctx context.Context) *zap.Logger {
	return z.With(Fields(ctx)...)
}

func New(root *zap.Logger) *Logger {
	return &Logger{
		root: root,
	}
}

type Logger struct {
	root *zap.Logger
}

func (l *Logger) Info(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(l.root, ctx).Info(msg, fields...)
}

func (l *Logger) Warn(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(l.root, ctx).Warn(msg, fields...)
}

func (l *Logger) Error(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(l.root, ctx).Error(msg, fields...)
}

func (l *Logger) Panic(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	GetLogger(l.root, ctx).Panic(msg, fields...)
}

func (l *Logger) IfErr(err error) *Logger {
	return l.With(zap.Error(err))
}

func (l *Logger) With(fields ...zap.Field) *Logger {
	if l == nil {
		return nil
	}
	return &Logger{
		root: l.root.With(fields...),
	}
}
