package logx

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestCloudLineHasSeverityMessageAndStack(t *testing.T) {
	var buf bytes.Buffer
	l := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(cloudEncoder()), zapcore.AddSync(&buf), zapcore.DebugLevel),
		zap.AddStacktrace(zapcore.ErrorLevel)).Sugar()
	l.Warnw("careful", "request_id", "abc")
	l.Errorw("boom", "err", errors.New("x"))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %s", len(lines), buf.String())
	}
	var w, e map[string]any
	json.Unmarshal([]byte(lines[0]), &w)
	json.Unmarshal([]byte(lines[1]), &e)
	if w["severity"] != "WARNING" || w["message"] != "careful" || w["request_id"] != "abc" {
		t.Errorf("warn line wrong: %v", w)
	}
	if e["severity"] != "ERROR" || e["stack_trace"] == nil {
		t.Errorf("error line should have severity ERROR and a stack_trace: %v", e)
	}
}

// log.level in config is a floor: nothing below it is written.
func TestLevelFromConfigIsAFloor(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  []string
	}{
		{"debug", []string{"DEBUG", "INFO", "WARNING", "ERROR"}},
		{"info", []string{"INFO", "WARNING", "ERROR"}},
		{"warn", []string{"WARNING", "ERROR"}},
		{"error", []string{"ERROR"}},
	} {
		var buf bytes.Buffer
		old := stdout
		stdout = zapcore.AddSync(&buf)
		cfg := viper.New()
		cfg.Set("log.level", tc.level) // app.dev unset: prod mode
		l, _, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		l.Debug("d")
		l.Info("i")
		l.Warn("w")
		l.Error("e")
		stdout = old

		var got []string
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			json.Unmarshal([]byte(line), &m)
			got = append(got, m["severity"].(string))
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("level %s: wrote %v, want %v", tc.level, got, tc.want)
		}
	}
	cfg := viper.New()
	cfg.Set("log.level", "chatty")
	if _, _, err := New(cfg); err == nil {
		t.Error("an unknown level must be refused")
	}
}
