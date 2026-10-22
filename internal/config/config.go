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
	"slices"
	"strings"
	"time"
)

type Config struct {
	App       App                 `json:"app"`
	Providers map[string]Provider `json:"providers"`
	API       API                 `json:"api"`
	Models    Models              `json:"models"`
	Reply     Reply               `json:"reply"`
	Chat      Conversation        `json:"conversation"`
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

// API controls who may call the Core API. Every request must carry a token
// signed with the shared secret and naming one of the listed adapters; anything
// else is refused.
type API struct {
	// SecretEnv names the env var holding the shared secret (32+ bytes; make one
	// with `openssl rand -hex 32`). All adapters use the same one.
	SecretEnv string `json:"secret_env"`
	// Adapters lists the adapters allowed to call the API (telegram, web...).
	Adapters []string `json:"adapters"`
	// TokenTTLSeconds is how long a minted token is valid. 0 means 300 (5 minutes).
	TokenTTLSeconds int `json:"token_ttl_seconds"`
	// AllowRemoteListen permits app.listen to be an address other machines can
	// reach. Off by default: Core refuses to start on one.
	AllowRemoteListen bool `json:"allow_remote_listen"`
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
	// CharsPerMinute is how much text becomes one minute of speech; used only
	// to estimate audio length before synthesis. 0 means the default (950).
	CharsPerMinute int `json:"chars_per_minute"`
	// AudioTokensPerSec converts seconds of generated speech to billed output
	// tokens (Gemini TTS bills 25 per second). 0 means 25.
	AudioTokensPerSec float64 `json:"audio_tokens_per_sec"`
}

// Reply controls how answers to follow-up questions are delivered.
type Reply struct {
	// Voice is the default: true answers with a voice note, false with text.
	// A user can ask for the other format in a message, for that turn only.
	Voice bool `json:"voice"`
}

// Conversation controls how a long conversation is kept short enough to send
// to the model on every turn.
type Conversation struct {
	// SummarizeOverChars: when the messages not yet summarised add up to more
	// than this many characters, the oldest are folded into a summary until what
	// is left is half this size. 0 turns summarising off. About 4 characters make
	// a token, so 100000 is roughly 25,000 tokens. The blog's own text is not counted.
	SummarizeOverChars int `json:"summarize_over_chars"`
}

type Cost struct {
	ConfirmOverUSD float64 `json:"confirm_over_usd"`
	// EstUSDPer1KChar is the fallback price for the pre-run estimate when the
	// model's price is not known (offline and never fetched).
	EstUSDPer1KChar float64 `json:"est_usd_per_1k_chars"`
	ShowInReply     bool    `json:"show_in_reply"`
	// PriceTTLDays is how long a fetched model price is trusted before it is
	// fetched again (and checked against a real charge). 0 means 7.
	PriceTTLDays int `json:"price_ttl_days"`
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
	need("api.secret_env", c.API.SecretEnv)
	if len(c.API.Adapters) == 0 {
		errs = append(errs, errors.New("api.adapters must list at least one adapter, or nothing could call the API"))
	}
	if c.API.TokenTTLSeconds < 0 {
		errs = append(errs, errors.New("api.token_ttl_seconds must be >= 0"))
	}
	if c.Telegram.Enabled {
		if !slices.Contains(c.API.Adapters, "telegram") {
			errs = append(errs, errors.New(`api.adapters needs "telegram" in it when telegram is enabled`))
		}
	}
	if c.Chat.SummarizeOverChars < 0 {
		errs = append(errs, errors.New("conversation.summarize_over_chars must be >= 0"))
	}
	if c.Cost.PriceTTLDays < 0 || c.Models.TTS.AudioTokensPerSec < 0 {
		errs = append(errs, errors.New("cost.price_ttl_days and models.tts.audio_tokens_per_sec must be >= 0"))
	}
	if c.Models.TTS.CharsPerMinute < 0 {
		errs = append(errs, errors.New("models.tts.chars_per_minute must be >= 0"))
	}
	needProvider("models.llm.provider", c.Models.LLM.Provider)
	need("models.llm.model", c.Models.LLM.Model)
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

// APISecret reads the shared API secret from the environment. The error names
// the env var that is missing, never a value.
func (c *Config) APISecret() ([]byte, error) {
	v, err := RequireKey("api.secret_env", c.API.SecretEnv)
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}

// TokenTTL is how long a minted API token lives.
func (c *Config) TokenTTL() time.Duration {
	if c.API.TokenTTLSeconds == 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.API.TokenTTLSeconds) * time.Second
}
