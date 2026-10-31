// Package config loads config.json into viper. There is no fixed struct: code
// reads what it needs by key (cfg.GetString("models.tts.model")), so adding a
// setting means editing the JSON and the code that uses it, nothing in between.
// Config never holds secrets; the file only names the secret that holds each key
// (see package secrets, which errors if one is missing).
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"
)

// Config is the parsed config.json.
type Config = viper.Viper

// Load reads config.json at path and applies defaults. It does not validate
// keys: the JSON shape changes often, so code that reads a key checks it there.
// Secrets are not loaded here (see secrets.Open).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w (copy config.example.json to config.json)", path, err)
	}
	c := viper.New()
	c.SetConfigType("json")
	setDefaults(c)
	if err := c.ReadConfig(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// setDefaults gives the keys that used to mean "0 means default" real values.
func setDefaults(c *Config) {
	c.SetDefault("api.token_ttl_seconds", 300)
	c.SetDefault("api.allow_remote_listen", false)
	c.SetDefault("models.tts.chars_per_minute", 950)
	c.SetDefault("models.tts.audio_tokens_per_sec", 25)
	c.SetDefault("cost.price_ttl_days", 7)
	// Idle window (package idle): minutes. Used only when app.dev is false.
	c.SetDefault("idle.initial_min", 2)
	c.SetDefault("idle.blog_min", 6)
	c.SetDefault("idle.first_answer_min", 10)
	c.SetDefault("idle.extend_min", 5)
	c.SetDefault("idle.cap_min", 90)
	c.SetDefault("idle.keepalive_min", 5)
	c.SetDefault("idle.keepalive_db", false)
}

// TokenTTL is how long a minted API token lives.
func TokenTTL(c *Config) time.Duration {
	return time.Duration(c.GetInt("api.token_ttl_seconds")) * time.Second
}

// AllowedUserIDs is telegram.allowed_user_ids as int64 (viper has no int64 slice getter).
func AllowedUserIDs(c *Config) []int64 {
	var ids []int64
	for _, id := range c.GetIntSlice("telegram.allowed_user_ids") {
		ids = append(ids, int64(id))
	}
	return ids
}
