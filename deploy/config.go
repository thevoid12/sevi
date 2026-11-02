package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// prodConfig is the config baked into the image; the deploy reads retention_days from it.
const prodConfig = "../config.prod.json"

// settings are the stack's configuration (`pulumi config set sevi:<key> ...`).
type settings struct {
	ProjectID           string // new GCP project to create
	BillingAccount      string // billing account the project is linked to
	Region              string // Cloud Run, Artifact Registry and API Gateway region
	CloudflareAccountID string
	R2Location          string // R2 location hint: wnam, enam, weur, eeur, apac
	AudioRetentionDays  int    // R2 deletes audio older than this; from retention_days in the prod config
	LogRetentionDays    int

	// Secrets: values only you have. Everything else in the secret JSON is
	// generated or comes from the other resources.
	OpenRouterAPIKey pulumi.StringOutput
	TelegramBotToken pulumi.StringOutput
	JinaAPIKey       pulumi.StringOutput // optional, may be empty
	DatabaseURL      pulumi.StringOutput // Neon connection string (see neon.json); created outside Pulumi
}

func loadSettings(ctx *pulumi.Context) (settings, error) {
	c := config.New(ctx, "sevi")
	s := settings{
		Region:           c.Get("region"),
		R2Location:       c.Get("r2Location"),
		LogRetentionDays: intOr(c.GetInt("logRetentionDays"), 7),
	}
	for name, dst := range map[string]*string{"projectId": &s.ProjectID, "billingAccount": &s.BillingAccount, "cloudflareAccountId": &s.CloudflareAccountID} {
		v, err := c.Try(name)
		if err != nil {
			return s, fmt.Errorf("set it with: pulumi config set sevi:%s <value>", name)
		}
		*dst = v
	}
	s.Region = strOr(s.Region, "us-central1") // API Gateway is not offered in every region
	s.R2Location = strOr(s.R2Location, "enam")
	days, err := audioRetentionDays(prodConfig)
	if err != nil {
		return s, err
	}
	s.AudioRetentionDays = days
	s.DatabaseURL = c.RequireSecret("databaseUrl")
	s.OpenRouterAPIKey = c.RequireSecret("openrouterApiKey")
	s.TelegramBotToken = c.RequireSecret("telegramBotToken")
	s.JinaAPIKey = c.GetSecret("jinaApiKey").ApplyT(func(v string) string { return v }).(pulumi.StringOutput)
	return s, nil
}

func intOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func strOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// audioRetentionDays reads retention_days from the prod config file, so the
// number lives in one place. Missing or not positive means 7.
func audioRetentionDays(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	var c struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.RetentionDays <= 0 {
		return 7, nil
	}
	return c.RetentionDays, nil
}
