// Package logx builds the app-wide zap sugared logger: human-readable console
// output, JSON lines in <dir>/sevi.log, and errors-and-above also in
// <dir>/error.log. Files rotate via lumberjack.
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

// New returns the logger and a flush function to defer from main.
func New(c *viper.Viper) (*zap.SugaredLogger, func(), error) {
	if err := os.MkdirAll(c.GetString("log.dir"), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir %s: %w", c.GetString("log.dir"), err)
	}
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(c.GetString("log.level")))); err != nil {
		return nil, nil, fmt.Errorf("log level %q: %w", c.GetString("log.level"), err)
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

// With returns log tagged with the request id from ctx, if any.
func With(ctx context.Context, log *zap.SugaredLogger) *zap.SugaredLogger {
	if id := reqid.From(ctx); id != "" {
		return log.With("request_id", id)
	}
	return log
}
