// The Pulumi program that builds all of sevi's infrastructure: the GCP project
// (Cloud Run, API Gateway, Secret Manager, Artifact Registry, log retention), the Cloudflare R2 audio bucket, and the Neon database.
//
//	make infra_up       build the binary and run `pulumi up`
//
// See README.md for the one-time setup (tokens and config).
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() { pulumi.Run(run) }

func run(ctx *pulumi.Context) error {
	s, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	g, err := newGoogle(ctx, s)
	if err != nil {
		return err
	}
	gen, err := newGenerated(ctx)
	if err != nil {
		return err
	}
	if err := newBudget(ctx, s, g); err != nil {
		return err
	}
	store, err := newR2(ctx, s)
	if err != nil {
		return err
	}

	// Version 1 of the secret: everything except the gateway URLs.
	base := map[string]pulumi.StringInput{
		"OPENROUTER_API_KEY":      s.OpenRouterAPIKey,
		"TELEGRAM_BOT_TOKEN":      s.TelegramBotToken,
		"JINA_API_KEY":            s.JinaAPIKey,
		"SEVI_JWT_SECRET":         gen.jwt,
		"TELEGRAM_WEBHOOK_SECRET": gen.webhook,
		"DATABASE_URL":            s.DatabaseURL,
		"R2_ENDPOINT":             store.endpoint,
		"R2_BUCKET":               store.bucket,
		"R2_ACCESS_KEY_ID":        store.accessKeyID,
		"R2_SECRET_ACCESS_KEY":    store.secretAccessKey,
	}
	v1, err := addVersion(ctx, g, "secret-v1", secretJSON(base))
	if err != nil {
		return err
	}

	// Create the schema before the app first starts. Re-runs when a migration changes.
	migrated, err := runMigrations(ctx, s.DatabaseURL)
	if err != nil {
		return err
	}

	image, err := buildImage(ctx, s, g)
	if err != nil {
		return err
	}
	svc, err := newService(ctx, s, g, image, v1, migrated)
	if err != nil {
		return err
	}
	gw, err := newGateway(ctx, s, g, svc)
	if err != nil {
		return err
	}

	// Version 2 adds the URLs that need the gateway: the app's keep-warm ping and
	// the Telegram webhook (the key rides in the query string).
	full := map[string]pulumi.StringInput{
		"KEEPALIVE_URL":        pulumi.Sprintf("https://%s/keepalive?key=%s", gw.host, gw.apiKey),
		"TELEGRAM_WEBHOOK_URL": pulumi.Sprintf("https://%s/telegram?key=%s", gw.host, gw.apiKey),
	}
	for k, v := range base {
		full[k] = v
	}
	v2, err := addVersion(ctx, g, "secret-v2", secretJSON(full), v1)
	if err != nil {
		return err
	}
	if err := registerWebhook(ctx, pulumi.Sprintf("%s/versions/latest", g.secret.Name),
		pulumi.Array{gw.host, gen.webhook}, v2); err != nil {
		return err
	}

	ctx.Export("project", g.project.ProjectId)
	ctx.Export("cloudRunUrl", svc.Uri)
	ctx.Export("gatewayHost", gw.host)
	ctx.Export("secretName", g.secret.Name)
	ctx.Export("image", image)
	return nil
}
