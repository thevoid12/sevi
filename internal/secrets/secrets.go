// Package secrets loads every secret a run needs in one call, as a viper
// instance, so adding a secret means editing JSON only: no struct, no code.
// Backends implement Store (local file, Google Secret Manager, later Cloudflare
// and others); all return the same JSON shape (see secret_sample.json).
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Store is one secret backend. Add a new backend by implementing it and adding
// a case to FromSettings.
type Store interface {
	// Kind names the backend ("local", "gcp", ...) for logs and errors.
	Kind() string
	// Load fetches every secret for this run, parsed from the shared JSON shape.
	Load(ctx context.Context) (*viper.Viper, error)
}

// Parse decodes a JSON payload with viper. A payload with no keys is refused
// so a wrong or empty secret fails at startup, not at first use.
func Parse(data []byte) (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigType("json")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("secrets payload must be a JSON object: %w", err)
	}
	if len(v.AllKeys()) == 0 {
		return nil, errors.New("secrets payload has no keys")
	}
	return v, nil
}

// Get returns the value and whether it is set and non-empty. Names are
// case-insensitive (viper's rule).
func Get(v *viper.Viper, name string) (string, bool) {
	if v == nil || name == "" {
		return "", false
	}
	val := strings.TrimSpace(v.GetString(name))
	return val, val != ""
}

// Has reports whether the name exists, even with an empty value.
func Has(v *viper.Viper, name string) bool { return v != nil && v.IsSet(name) }

// Require is Get that fails with an error naming the config field and secret.
func Require(v *viper.Viper, field, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%s: no secret name set in config", field)
	}
	val, ok := Get(v, name)
	if !ok {
		return "", fmt.Errorf("%s: secret %s is not set (add it to the secrets file or secret)", field, name)
	}
	return val, nil
}

// FromSettings picks the backend from settings (the parsed config.json):
// app.dev true -> local file at secrets.file (default secret.json);
// otherwise Google Secret Manager at secrets.name.
func FromSettings(s *viper.Viper) (Store, error) {
	if s.GetBool("app.dev") {
		path := s.GetString("secrets.file")
		if path == "" {
			path = "secret.json"
		}
		return Local{Path: path}, nil
	}
	name := s.GetString("secrets.name")
	if name == "" {
		return nil, errors.New("secrets.name is required when app.dev is false")
	}
	return GCP{Name: name}, nil
}

// Open reads the settings from the config file at configPath, picks the
// backend and loads every secret.
func Open(ctx context.Context, configPath string) (Store, *viper.Viper, error) {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", configPath, err)
	}
	s := viper.New()
	s.SetConfigType("json")
	if err := s.ReadConfig(bytes.NewReader(b)); err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", configPath, err)
	}
	// The deploy (Pulumi) knows the project, so it passes the secret's name in the
	// environment instead of baking it into the image's config file.
	_ = s.BindEnv("secrets.name", "SEVI_SECRETS_NAME")
	st, err := FromSettings(s)
	if err != nil {
		return nil, nil, err
	}
	v, err := st.Load(ctx)
	if err != nil {
		return st, nil, fmt.Errorf("%s secrets: %w", st.Kind(), err)
	}
	return st, v, nil
}
