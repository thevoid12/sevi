package main

import (
	"encoding/json"

	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// generated are the secrets sevi invents for itself; you never see or type them.
type generated struct {
	jwt, webhook pulumi.StringOutput
}

func newGenerated(ctx *pulumi.Context) (*generated, error) {
	jwt, err := random.NewRandomPassword(ctx, "jwt-secret", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)})
	if err != nil {
		return nil, err
	}
	// Letters and digits only: Telegram accepts just A-Z a-z 0-9 _ - in a webhook secret.
	hook, err := random.NewRandomPassword(ctx, "webhook-secret", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)})
	if err != nil {
		return nil, err
	}
	return &generated{jwt: jwt.Result, webhook: hook.Result}, nil
}

// secretJSON renders the object sevi reads at startup (see secret_sample.json:
// the names are the source of truth). Values are Pulumi outputs, so the result is secret.
func secretJSON(vals map[string]pulumi.StringInput) pulumi.StringOutput {
	in := pulumi.StringMap{}
	for k, v := range vals {
		in[k] = v
	}
	return in.ToStringMapOutput().ApplyT(func(m map[string]string) (string, error) {
		b, err := json.MarshalIndent(m, "", "  ")
		return string(b), err
	}).(pulumi.StringOutput)
}

// addVersion stores one version of the secret. Cloud Run must find the secret when it
// first starts, but the gateway URLs only exist after Cloud Run does, so there are
// two versions: the first has everything but the URLs, the second adds them.
// "latest" is the second from then on.
func addVersion(ctx *pulumi.Context, g *google, name string, data pulumi.StringOutput, after ...pulumi.Resource) (*secretmanager.SecretVersion, error) {
	return secretmanager.NewSecretVersion(ctx, name, &secretmanager.SecretVersionArgs{
		Secret: g.secret.ID(), SecretData: pulumi.ToSecret(data).(pulumi.StringOutput),
	}, pulumi.DependsOn(after))
}
