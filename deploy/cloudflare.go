package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// The typed Cloudflare SDK is one 430,000-line Go package that needs over 10 GB
// of memory to compile. Resources and data sources are registered by their type
// token instead (the provider plugin is the same; only the Go wrapper is skipped).
const cloudflareVersion = "6.23.2"

type cfResource struct{ pulumi.CustomResourceState }

type cfApiToken struct {
	pulumi.CustomResourceState
	Value pulumi.StringOutput `pulumi:"value"`
}

func cfOpts(opts ...pulumi.ResourceOption) []pulumi.ResourceOption {
	return append([]pulumi.ResourceOption{pulumi.Version(cloudflareVersion)}, opts...)
}

// r2 is the audio bucket and the S3 credentials sevi uses to reach it.
type r2 struct {
	endpoint, bucket pulumi.StringOutput
	accessKeyID      pulumi.StringOutput
	secretAccessKey  pulumi.StringOutput
}

// newR2 creates the bucket, a lifecycle rule that expires old audio, and an API
// token limited to this bucket. R2 derives S3 credentials from a token: the
// access key id is the token's id, the secret is the SHA-256 of the token value.
func newR2(ctx *pulumi.Context, s settings) (*r2, error) {
	const bucketName = "sevi-audio"
	acct := pulumi.String(s.CloudflareAccountID)

	var bucket cfResource
	if err := ctx.RegisterResource("cloudflare:index/r2Bucket:R2Bucket", "audio", pulumi.Map{
		"accountId": acct, "name": pulumi.String(bucketName), "location": pulumi.String(s.R2Location),
	}, &bucket, cfOpts()...); err != nil {
		return nil, err
	}
	var lifecycle cfResource
	if err := ctx.RegisterResource("cloudflare:index/r2BucketLifecycle:R2BucketLifecycle", "audio-lifecycle", pulumi.Map{
		"accountId": acct, "bucketName": pulumi.String(bucketName),
		"rules": pulumi.Array{pulumi.Map{
			"id": pulumi.String("expire-old-audio"), "enabled": pulumi.Bool(true),
			"conditions": pulumi.Map{"prefix": pulumi.String("")},
			"deleteObjectsTransition": pulumi.Map{"condition": pulumi.Map{
				"type": pulumi.String("Age"), "maxAge": pulumi.Int(s.AudioRetentionDays * 24 * 3600),
			}},
		}},
	}, &lifecycle, cfOpts(pulumi.DependsOn([]pulumi.Resource{&bucket}))...); err != nil {
		return nil, err
	}

	read, err := permissionGroup(ctx, "Workers R2 Storage Bucket Item Read")
	if err != nil {
		return nil, err
	}
	write, err := permissionGroup(ctx, "Workers R2 Storage Bucket Item Write")
	if err != nil {
		return nil, err
	}
	var token cfApiToken
	if err := ctx.RegisterResource("cloudflare:index/apiToken:ApiToken", "audio-token", pulumi.Map{
		"name": pulumi.String("sevi-audio-s3"),
		"policies": pulumi.Array{pulumi.Map{
			"effect": pulumi.String("allow"),
			"permissionGroups": pulumi.Array{
				pulumi.Map{"id": pulumi.String(read)},
				pulumi.Map{"id": pulumi.String(write)},
			},
			"resources": pulumi.Sprintf(`{"com.cloudflare.edge.r2.bucket.%s_default_%s":"*"}`, s.CloudflareAccountID, bucketName),
		}},
	}, &token, cfOpts(pulumi.DependsOn([]pulumi.Resource{&bucket}))...); err != nil {
		return nil, err
	}
	secret := token.Value.ApplyT(func(v string) string {
		sum := sha256.Sum256([]byte(v))
		return hex.EncodeToString(sum[:])
	}).(pulumi.StringOutput)
	return &r2{
		endpoint:        pulumi.Sprintf("%s.r2.cloudflarestorage.com", s.CloudflareAccountID),
		bucket:          pulumi.String(bucketName).ToStringOutput(),
		accessKeyID:     token.ID().ToStringOutput(),
		secretAccessKey: pulumi.ToSecret(secret).(pulumi.StringOutput),
	}, nil
}

// permissionGroup finds a Cloudflare API token permission group's id by its exact name.
func permissionGroup(ctx *pulumi.Context, name string) (string, error) {
	type group struct {
		Id   string `pulumi:"id"`
		Name string `pulumi:"name"`
	}
	var res struct {
		Results []group `pulumi:"results"`
	}
	args := struct {
		Name string `pulumi:"name"`
	}{Name: name}
	if err := ctx.Invoke("cloudflare:index/getApiTokenPermissionGroupsList:getApiTokenPermissionGroupsList", args, &res, pulumi.Version(cloudflareVersion)); err != nil {
		return "", fmt.Errorf("look up permission group %q: %w", name, err)
	}
	for _, g := range res.Results {
		if g.Name == name {
			return g.Id, nil
		}
	}
	return "", fmt.Errorf("Cloudflare has no permission group named %q", name)
}
