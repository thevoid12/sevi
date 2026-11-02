package main

import (
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// registerWebhook runs `sevi telegram set-webhook` (the adapter's own tool) once
// the final secret exists, so Telegram points at the gateway. It reads the bot
// token, webhook secret and URL from Secret Manager, so it needs your gcloud
// application-default credentials (`gcloud auth application-default login`).
// It re-runs when the gateway URL or the webhook secret changes, and removes the
// webhook on `pulumi destroy`.
func registerWebhook(ctx *pulumi.Context, secretName pulumi.StringInput, triggers pulumi.Array, after ...pulumi.Resource) error {
	env := pulumi.StringMap{"SEVI_SECRETS_NAME": secretName}
	_, err := local.NewCommand(ctx, "set-webhook", &local.CommandArgs{
		Dir:         pulumi.String(".."),
		Create:      pulumi.String("go run ./cmd/sevi -config config.prod.json telegram set-webhook"),
		Delete:      pulumi.String("go run ./cmd/sevi -config config.prod.json telegram delete-webhook"),
		Environment: env,
		Triggers:    triggers,
	}, pulumi.DependsOn(after))
	return err
}
