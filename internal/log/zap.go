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

func fields(ctx context.Context) []zap.Field {
	existingFieldsVal := ctx.Value(loggerKey)
	if existingFieldsVal == nil {
		return nil
	}
	return existingFieldsVal.([]zap.Field)
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

func (l *Logger) logger(ctx context.Context) *zap.Logger {
	allFields := fields(ctx)
	for _, d := range l.dynamicFields {
		allFields = append(allFields, d(ctx)...)
	}
	return l.root.With(allFields...).WithOptions(zap.AddCallerSkip(2))
}

func (l *Logger) Info(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.root.WithOptions(zap.Hooks())
	l.logger(ctx).Info(msg, fields...)
}

func (l *Logger) Warn(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.logger(ctx).Warn(msg, fields...)
}

func (l *Logger) Error(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.logger(ctx).Error(msg, fields...)
}

func (l *Logger) Debug(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.logger(ctx).Debug(msg, fields...)
}

func (l *Logger) Panic(ctx context.Context, msg string, fields ...zap.Field) {
	if l == nil {
		return
	}
	l.logger(ctx).Panic(msg, fields...)
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
		root:          l.root.With(fields...),
		dynamicFields: l.dynamicFields,
	}
}

type FieldLogger struct {
	Logger *Logger
}

func (f *FieldLogger) Log(keyvals ...interface{}) {
	if len(keyvals) == 0 {
		return
	}
	var fields []zap.Field
	msg := "log sent"
	for i := 0; i < len(keyvals); i += 2 {
		if i+1 >= len(keyvals) {
			continue
		}
		keyInt := keyvals[i]
		v := keyvals[i+1]
		if keyAsString, ok := keyInt.(string); ok {
			if keyAsString == "msg" {
				if valAsString, ok := v.(string); ok {
					msg = valAsString
				}
			} else {
				fields = append(fields, zap.Any(keyAsString, v))
			}
		}
	}
	f.Logger.Info(context.Background(), msg, fields...)
}
