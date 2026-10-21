// Package config loads config.json and resolves API keys from environment
// variables. Everything tunable lives in config.json; secrets never do (the
// file only names the env var that holds each key).
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	App       App                 `json:"app"`
	Providers map[string]Provider `json:"providers"`
	Models    Models              `json:"models"`
	Cost      Cost                `json:"cost"`
	Telegram  Telegram            `json:"telegram"`
	Log       Log                 `json:"log"`
}

type App struct {
	Listen    string `json:"listen"`
	DataDir   string `json:"data_dir"`
	OutputDir string `json:"output_dir"`
	Retries   int    `json:"retries"` // extra attempts for any failed provider call
}

// Provider is one API endpoint and the env var holding its key. Models refer
// to providers by name, so a key and URL are written once.
type Provider struct {
	BaseURL string `json:"base_url"`
	KeyEnv  string `json:"key_env"`
}

type Models struct {
	LLM LLM  `json:"llm"`
	STT *STT `json:"stt"` // omit to send audio straight to the LLM
	TTS TTS  `json:"tts"`
}

type LLM struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

type STT struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type TTS struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Voice      string `json:"voice"`
	ChunkChars int    `json:"chunk_chars"`
}

type Cost struct {
	ConfirmOverUSD  float64 `json:"confirm_over_usd"`
	EstUSDPer1KChar float64 `json:"est_usd_per_1k_chars"` // used until the ledger has history
	ShowInReply     bool    `json:"show_in_reply"`
}

type Telegram struct {
	Enabled        bool    `json:"enabled"`
	BotTokenEnv    string  `json:"bot_token_env"`
	AllowedUserIDs []int64 `json:"allowed_user_ids"`
}

type Log struct {
	Level      string `json:"level"`
	Dir        string `json:"dir"`
	MaxSizeMB  int    `json:"max_size_mb"`
	MaxBackups int    `json:"max_backups"`
	MaxAgeDays int    `json:"max_age_days"`
}

// Provider returns the named provider (validated to exist at load time).
func (c *Config) Provider(name string) Provider { return c.Providers[name] }

// Load reads .env (if present, without overriding real env vars), then
// config.json at path, and validates structure. Keys are not checked here;
// use RequireKey for the ones a given run needs.
func Load(path string) (*Config, error) {
	loadDotEnv(".env")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w (copy config.example.json to config.json)", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // catch typos in key names
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	need := func(name, v string) {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	needProvider := func(field, name string) {
		if _, ok := c.Providers[name]; !ok {
			errs = append(errs, fmt.Errorf("%s: unknown provider %q (define it under providers)", field, name))
		}
	}
	need("app.listen", c.App.Listen)
	need("app.data_dir", c.App.DataDir)
	need("app.output_dir", c.App.OutputDir)
	if c.App.Retries < 0 {
		errs = append(errs, errors.New("app.retries must be >= 0"))
	}
	for name, p := range c.Providers {
		need("providers."+name+".base_url", p.BaseURL)
		need("providers."+name+".key_env", p.KeyEnv)
	}
	needProvider("models.tts.provider", c.Models.TTS.Provider)
	need("models.tts.model", c.Models.TTS.Model)
	need("models.tts.voice", c.Models.TTS.Voice)
	if c.Models.TTS.ChunkChars <= 0 {
		errs = append(errs, errors.New("models.tts.chunk_chars must be > 0"))
	}
	needProvider("models.llm.provider", c.Models.LLM.Provider)
	if c.Models.STT != nil {
		needProvider("models.stt.provider", c.Models.STT.Provider)
	}
	if c.Cost.ConfirmOverUSD < 0 {
		errs = append(errs, errors.New("cost.confirm_over_usd must be >= 0"))
	}
	if c.Telegram.Enabled {
		need("telegram.bot_token_env", c.Telegram.BotTokenEnv)
		if len(c.Telegram.AllowedUserIDs) == 0 {
			errs = append(errs, errors.New("telegram.allowed_user_ids must not be empty when telegram is enabled"))
		}
	}
	need("log.dir", c.Log.Dir)
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be debug|info|warn|error, got %q", c.Log.Level))
	}
	return errors.Join(errs...)
}

// RequireKey returns the value of the env var named in config, or an error
// that names both the config field and the missing variable.
func RequireKey(field, envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("%s: no env var name set in config", field)
	}
	v := os.Getenv(envName)
	if v == "" {
		return "", fmt.Errorf("%s: environment variable %s is not set (put it in .env or export it)", field, envName)
	}
	return v, nil
}

// ProviderKey returns the key for a named provider, or an error naming the
// missing env var.
func (c *Config) ProviderKey(name string) (string, error) {
	return RequireKey("providers."+name+".key_env", c.Providers[name].KeyEnv)
}

// KeyStatus reports whether the key of each provider used by a model is
// present, never its value. Other providers (e.g. jina) have optional keys.
func (c *Config) KeyStatus() map[string]bool {
	s := map[string]bool{}
	used := []string{c.Models.LLM.Provider, c.Models.TTS.Provider}
	if c.Models.STT != nil {
		used = append(used, c.Models.STT.Provider)
	}
	for _, name := range used {
		s["providers."+name] = os.Getenv(c.Providers[name].KeyEnv) != ""
	}
	if c.Telegram.Enabled {
		s["telegram.bot_token_env"] = os.Getenv(c.Telegram.BotTokenEnv) != ""
	}
	return s
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && os.Getenv(k) == "" {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}
