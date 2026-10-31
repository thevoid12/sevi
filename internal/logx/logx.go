// Package logx builds the app-wide zap sugared logger.
//
// Dev (app.dev true): human-readable console output, JSON lines in
// <dir>/sevi.log, and errors-and-above also in <dir>/error.log (lumberjack rotation).
//
// Prod: one JSON object per line on stdout and nothing else. Cloud Run sends
// stdout to Cloud Logging, which reads "severity" and "message" from the line;
// files would only fill the instance's memory.
package logx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"sevi/internal/reqid"
)

// stdout is where prod logs go; a variable so tests can capture it.
var stdout zapcore.WriteSyncer = zapcore.Lock(os.Stdout)

// New returns the logger and a flush function to defer from main.
func New(c *viper.Viper) (*zap.SugaredLogger, func(), error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(c.GetString("log.level")))); err != nil {
		return nil, nil, fmt.Errorf("log level %q: %w", c.GetString("log.level"), err)
	}
	if !c.GetBool("app.dev") {
		l := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(cloudEncoder()), stdout, lvl),
			zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)).Sugar()
		return l, func() { _ = l.Sync() }, nil
	}
	if err := os.MkdirAll(c.GetString("log.dir"), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir %s: %w", c.GetString("log.dir"), err)
	}

	rotator := func(name string) zapcore.WriteSyncer {
		return zapcore.AddSync(&lumberjack.Logger{
			Filename:   filepath.Join(c.GetString("log.dir"), name),
			MaxSize:    c.GetInt("log.max_size_mb"),
			MaxBackups: c.GetInt("log.max_backups"),
			MaxAge:     c.GetInt("log.max_age_days"),
			Compress:   true,
		})
	}

	consoleEnc := zap.NewDevelopmentEncoderConfig()
	consoleEnc.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEnc.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
	jsonEnc := zap.NewProductionEncoderConfig()
	jsonEnc.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewTee(
		zapcore.NewCore(zapcore.NewConsoleEncoder(consoleEnc), zapcore.Lock(os.Stdout), lvl),
		zapcore.NewCore(zapcore.NewJSONEncoder(jsonEnc), rotator("sevi.log"), lvl),
		zapcore.NewCore(zapcore.NewJSONEncoder(jsonEnc), rotator("error.log"), zapcore.ErrorLevel),
	)
	l := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)).Sugar()
	return l, func() { _ = l.Sync() }, nil
}

// cloudEncoder names the JSON fields the way Cloud Logging expects: severity and
// message are lifted into the log entry; a stack trace goes in stack_trace.
func cloudEncoder() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "severity",
		MessageKey:     "message",
		CallerKey:      "caller",
		StacktraceKey:  "stack_trace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeLevel:    cloudLevel,
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
	}
}

// cloudLevel maps zap levels to Cloud Logging severity names.
func cloudLevel(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	switch l {
	case zapcore.DebugLevel:
		enc.AppendString("DEBUG")
	case zapcore.InfoLevel:
		enc.AppendString("INFO")
	case zapcore.WarnLevel:
		enc.AppendString("WARNING")
	case zapcore.ErrorLevel:
		enc.AppendString("ERROR")
	case zapcore.DPanicLevel:
		enc.AppendString("CRITICAL")
	default:
		enc.AppendString("ALERT")
	}
}

// With returns log tagged with the request id from ctx, if any.
func With(ctx context.Context, log *zap.SugaredLogger) *zap.SugaredLogger {
	if id := reqid.From(ctx); id != "" {
		return log.With("request_id", id)
	}
	return log
}
