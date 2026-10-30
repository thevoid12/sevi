package secrets

import (
	"context"
	"fmt"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/spf13/viper"
)

// GCP is the prod backend. Name is the full version resource, e.g.
// projects/my-proj/secrets/sevi/versions/latest. Credentials come from the
// environment (the Cloud Run service account, or gcloud ADC locally).
type GCP struct{ Name string }

func (GCP) Kind() string { return "gcp" }

func (g GCP) Load(ctx context.Context) (*viper.Viper, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("secret manager client: %w", err)
	}
	defer client.Close()
	resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: g.Name})
	if err != nil {
		return nil, fmt.Errorf("access secret %s: %w", g.Name, err)
	}
	return Parse(resp.GetPayload().GetData())
}
