package main

import (
	"github.com/pulumi/pulumi-docker-build/sdk/go/dockerbuild"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/organizations"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// buildImage builds ../Dockerfile (which copies the prebuilt bin/sevi, so run
// `make build_linux` first; `make infra_up` does) and pushes it to Artifact Registry.
func buildImage(ctx *pulumi.Context, s settings, g *google) (pulumi.StringOutput, error) {
	token, err := organizations.GetClientConfig(ctx, nil, nil)
	if err != nil {
		return pulumi.StringOutput{}, err
	}
	host := s.Region + "-docker.pkg.dev"
	repo := pulumi.Sprintf("%s/%s/%s/sevi", host, g.project.ProjectId, g.registry.RepositoryId)
	img, err := dockerbuild.NewImage(ctx, "image", &dockerbuild.ImageArgs{
		Tags:       pulumi.StringArray{pulumi.Sprintf("%s:latest", repo)},
		Context:    &dockerbuild.BuildContextArgs{Location: pulumi.String("..")},
		Dockerfile: &dockerbuild.DockerfileArgs{Location: pulumi.String("../Dockerfile")},
		Platforms:  dockerbuild.PlatformArray{dockerbuild.Platform_Linux_amd64},
		Push:       pulumi.Bool(true),
		Registries: dockerbuild.RegistryArray{&dockerbuild.RegistryArgs{
			Address: pulumi.String(host), Username: pulumi.String("oauth2accesstoken"), Password: pulumi.String(token.AccessToken),
		}},
	}, pulumi.DependsOn(g.apis))
	if err != nil {
		return pulumi.StringOutput{}, err
	}
	return img.Ref, nil // includes the digest, so every new build is a new revision
}

// newService runs the image on Cloud Run: scale to zero, one instance (a single
// writer, and a ceiling on cost), CPU always on so a job keeps running after
// the webhook is answered, and no public access (only the gateway may call it).
func newService(ctx *pulumi.Context, s settings, g *google, image pulumi.StringOutput, after ...pulumi.Resource) (*cloudrunv2.Service, error) {
	svc, err := cloudrunv2.NewService(ctx, "service", &cloudrunv2.ServiceArgs{
		Project: g.project.ProjectId, Location: pulumi.String(s.Region), Name: pulumi.String("sevi"),
		Ingress:            pulumi.String("INGRESS_TRAFFIC_ALL"),
		DeletionProtection: pulumi.Bool(false),
		Template: &cloudrunv2.ServiceTemplateArgs{
			ServiceAccount: g.runSA.Email,
			Timeout:        pulumi.String("3600s"),
			Scaling:        &cloudrunv2.ServiceTemplateScalingArgs{MinInstanceCount: pulumi.Int(0), MaxInstanceCount: pulumi.Int(1)},
			Containers: cloudrunv2.ServiceTemplateContainerArray{&cloudrunv2.ServiceTemplateContainerArgs{
				Image: image,
				Ports: &cloudrunv2.ServiceTemplateContainerPortsArgs{ContainerPort: pulumi.Int(8080)},
				Envs: cloudrunv2.ServiceTemplateContainerEnvArray{
					&cloudrunv2.ServiceTemplateContainerEnvArgs{
						Name: pulumi.String("SEVI_SECRETS_NAME"), Value: pulumi.Sprintf("%s/versions/latest", g.secret.Name),
					},
					// A soft limit under the 1 GiB container: near it the Go runtime collects
					// garbage harder instead of letting a long blog's audio push the process over.
					&cloudrunv2.ServiceTemplateContainerEnvArgs{Name: pulumi.String("GOMEMLIMIT"), Value: pulumi.String("800MiB")},
				},
				Resources: &cloudrunv2.ServiceTemplateContainerResourcesArgs{
					Limits:          pulumi.StringMap{"cpu": pulumi.String("1"), "memory": pulumi.String("1Gi")},
					CpuIdle:         pulumi.Bool(false), // CPU always allocated (instance-based billing)
					StartupCpuBoost: pulumi.Bool(true),
				},
			}},
		},
	}, pulumi.DependsOn(append(append([]pulumi.Resource{}, g.apis...), after...)))
	if err != nil {
		return nil, err
	}
	return svc, nil
}
