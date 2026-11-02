package main

import (
	"encoding/base64"
	"os"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/apigateway"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/projects"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// edge is the public front door: the gateway host and the API key it demands.
type edge struct {
	host   pulumi.StringOutput // e.g. sevi-xxxx.uc.gateway.dev
	apiKey pulumi.StringOutput
}

// newGateway puts API Gateway in front of Cloud Run. The spec (openapi.yaml) lists
// only /telegram, which needs the API key; the gateway calls Cloud
// Run as its own service account, which alone may invoke the service.
func newGateway(ctx *pulumi.Context, s settings, g *google, svc *cloudrunv2.Service) (*edge, error) {
	if _, err := cloudrunv2.NewServiceIamMember(ctx, "gateway-invoker", &cloudrunv2.ServiceIamMemberArgs{
		Project: g.project.ProjectId, Location: pulumi.String(s.Region), Name: svc.Name,
		Role: pulumi.String("roles/run.invoker"), Member: pulumi.Sprintf("serviceAccount:%s", g.gwSA.Email),
	}); err != nil {
		return nil, err
	}

	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		return nil, err
	}
	rendered := svc.Uri.ApplyT(func(uri string) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(string(spec), "CLOUD_RUN_URL", strings.TrimRight(uri, "/"))))
	}).(pulumi.StringOutput)

	api, err := apigateway.NewApi(ctx, "api", &apigateway.ApiArgs{
		Project: g.project.ProjectId, ApiId: pulumi.String("sevi"),
	}, pulumi.DependsOn(g.apis))
	if err != nil {
		return nil, err
	}
	// A prefix, not a fixed id: a changed spec makes a new config and then retires the old one.
	cfg, err := apigateway.NewApiConfig(ctx, "api-config", &apigateway.ApiConfigArgs{
		Project: g.project.ProjectId, Api: api.ApiId, ApiConfigIdPrefix: pulumi.String("sevi-"),
		OpenapiDocuments: apigateway.ApiConfigOpenapiDocumentArray{&apigateway.ApiConfigOpenapiDocumentArgs{
			Document: &apigateway.ApiConfigOpenapiDocumentDocumentArgs{Path: pulumi.String("openapi.yaml"), Contents: rendered},
		}},
		GatewayConfig: &apigateway.ApiConfigGatewayConfigArgs{
			BackendConfig: &apigateway.ApiConfigGatewayConfigBackendConfigArgs{GoogleServiceAccount: g.gwSA.Name},
		},
	})
	if err != nil {
		return nil, err
	}
	gw, err := apigateway.NewGateway(ctx, "gateway", &apigateway.GatewayArgs{
		Project: g.project.ProjectId, Region: pulumi.String(s.Region), GatewayId: pulumi.String("sevi"),
		ApiConfig: cfg.ID(),
	})
	if err != nil {
		return nil, err
	}

	// The gateway's managed service must be enabled on the project, or every call
	// fails with 403 SERVICE_NOT_ACTIVATED.
	managed, err := projects.NewService(ctx, "api-managed-service", &projects.ServiceArgs{
		Project: g.project.ProjectId, Service: api.ManagedService, DisableOnDestroy: pulumi.Bool(false),
	}, pulumi.DependsOn([]pulumi.Resource{gw}))
	if err != nil {
		return nil, err
	}

	// The API key works only for this API, and is the one value that must stay secret.
	key, err := projects.NewApiKey(ctx, "api-key", &projects.ApiKeyArgs{
		Project: g.project.ProjectId, Name: pulumi.String("sevi-telegram"), DisplayName: pulumi.String("sevi telegram webhook"),
		Restrictions: &projects.ApiKeyRestrictionsArgs{ApiTargets: projects.ApiKeyRestrictionsApiTargetArray{
			&projects.ApiKeyRestrictionsApiTargetArgs{Service: api.ManagedService},
		}},
	}, pulumi.DependsOn(append(append([]pulumi.Resource{}, g.apis...), managed)))
	if err != nil {
		return nil, err
	}
	return &edge{host: gw.DefaultHostname, apiKey: pulumi.ToSecret(key.KeyString).(pulumi.StringOutput)}, nil
}
