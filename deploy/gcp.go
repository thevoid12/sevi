package main

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/logging"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// google holds the project and the pieces every other step builds on.
type google struct {
	project  *organizations.Project
	apis     []pulumi.Resource // depend on these before using a service
	registry *artifactregistry.Repository
	runSA    *serviceaccount.Account // Cloud Run runs as this
	gwSA     *serviceaccount.Account // API Gateway calls Cloud Run as this
	secret   *secretmanager.Secret
}

// The services the stack uses. All are free to enable.
var apiList = []string{
	"cloudresourcemanager.googleapis.com", "serviceusage.googleapis.com", "iam.googleapis.com",
	"run.googleapis.com", "artifactregistry.googleapis.com", "secretmanager.googleapis.com",
	"apigateway.googleapis.com", "servicemanagement.googleapis.com", "servicecontrol.googleapis.com",
	"apikeys.googleapis.com", "logging.googleapis.com", "billingbudgets.googleapis.com",
}

func newGoogle(ctx *pulumi.Context, s settings) (*google, error) {
	g := &google{}
	var err error
	g.project, err = organizations.NewProject(ctx, "project", &organizations.ProjectArgs{
		ProjectId:         pulumi.String(s.ProjectID),
		Name:              pulumi.String(s.ProjectID),
		BillingAccount:    pulumi.String(s.BillingAccount),
		AutoCreateNetwork: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	id := g.project.ProjectId

	for _, api := range apiList {
		svc, err := projects.NewService(ctx, "api-"+api, &projects.ServiceArgs{
			Project: id, Service: pulumi.String(api), DisableOnDestroy: pulumi.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		g.apis = append(g.apis, svc)
	}
	after := pulumi.DependsOn(g.apis)

	// Keep the last two images (the free tier is 0.5 GB) and delete everything older.
	g.registry, err = artifactregistry.NewRepository(ctx, "registry", &artifactregistry.RepositoryArgs{
		Project: id, Location: pulumi.String(s.Region), RepositoryId: pulumi.String("sevi"),
		Format: pulumi.String("DOCKER"), Description: pulumi.String("sevi images"),
		CleanupPolicies: artifactregistry.RepositoryCleanupPolicyArray{
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id: pulumi.String("keep-last-2"), Action: pulumi.String("KEEP"),
				MostRecentVersions: &artifactregistry.RepositoryCleanupPolicyMostRecentVersionsArgs{KeepCount: pulumi.Int(2)},
			},
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id: pulumi.String("delete-the-rest"), Action: pulumi.String("DELETE"),
				Condition: &artifactregistry.RepositoryCleanupPolicyConditionArgs{TagState: pulumi.String("ANY")},
			},
		},
	}, after)
	if err != nil {
		return nil, err
	}

	// Logs: keep seven days (shorter than the 30-day default, so never billed).
	if _, err := logging.NewProjectBucketConfig(ctx, "log-retention", &logging.ProjectBucketConfigArgs{
		Project: id, Location: pulumi.String("global"), BucketId: pulumi.String("_Default"),
		RetentionDays: pulumi.Int(s.LogRetentionDays),
	}, after); err != nil {
		return nil, err
	}

	if g.runSA, err = newAccount(ctx, "runtime", "sevi-run", "sevi Cloud Run service", id, after); err != nil {
		return nil, err
	}
	if g.gwSA, err = newAccount(ctx, "gateway", "sevi-gateway", "sevi API Gateway to Cloud Run", id, after); err != nil {
		return nil, err
	}

	// One secret holds every key as JSON (one secret, not one per key: free tier).
	g.secret, err = secretmanager.NewSecret(ctx, "secret", &secretmanager.SecretArgs{
		Project: id, SecretId: pulumi.String("sevi"),
		Replication: &secretmanager.SecretReplicationArgs{Auto: &secretmanager.SecretReplicationAutoArgs{}},
	}, after)
	if err != nil {
		return nil, err
	}
	if _, err := secretmanager.NewSecretIamMember(ctx, "secret-reader", &secretmanager.SecretIamMemberArgs{
		Project: id, SecretId: g.secret.SecretId, Role: pulumi.String("roles/secretmanager.secretAccessor"),
		Member: pulumi.Sprintf("serviceAccount:%s", g.runSA.Email),
	}); err != nil {
		return nil, err
	}
	return g, nil
}

func newAccount(ctx *pulumi.Context, name, accountID, display string, project pulumi.StringInput, opts ...pulumi.ResourceOption) (*serviceaccount.Account, error) {
	a, err := serviceaccount.NewAccount(ctx, name, &serviceaccount.AccountArgs{
		Project: project, AccountId: pulumi.String(accountID), DisplayName: pulumi.String(display),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("service account %s: %w", name, err)
	}
	return a, nil
}
