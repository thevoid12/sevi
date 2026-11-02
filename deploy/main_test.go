package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// mocks stands in for every provider, so the whole program runs with no cloud
// accounts. It records what the program registered.
type mocks struct {
	mu        sync.Mutex
	resources []mockResource
}

type mockResource struct {
	token, name string
	in          resource.PropertyMap
}

// extras are the values a real provider computes, which other resources read.
var extras = map[string]map[string]string{
	"gcp:organizations/project:Project":          {"number": "123456"},
	"gcp:cloudrunv2/service:Service":             {"uri": "https://sevi-abc-uc.a.run.app", "name": "sevi"},
	"gcp:apigateway/api:Api":                     {"managedService": "sevi-abc.apigateway.demo.cloud.goog"},
	"gcp:apigateway/gateway:Gateway":             {"defaultHostname": "sevi-x.uc.gateway.dev"},
	"gcp:projects/apiKey:ApiKey":                 {"keyString": "KEY123"},
	"gcp:secretmanager/secret:Secret":            {"name": "projects/123456/secrets/sevi"},
	"gcp:serviceaccount/account:Account":         {"email": "sa@demo.iam.gserviceaccount.com", "name": "projects/demo/serviceAccounts/sa"},
	"random:index/randomPassword:RandomPassword": {"result": "RANDOMVALUE"},
	"cloudflare:index/apiToken:ApiToken":         {"value": "cf-token-value"},
	"docker-build:index:Image":                   {"ref": "us-central1-docker.pkg.dev/demo/sevi/sevi@sha256:abc"},
}

func (m *mocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, mockResource{a.TypeToken, a.Name, a.Inputs})
	m.mu.Unlock()
	out := a.Inputs.Copy()
	for k, v := range extras[a.TypeToken] {
		out[resource.PropertyKey(k)] = resource.NewStringProperty(v)
	}
	if a.TypeToken == "gcp:serviceaccount/account:Account" {
		out["email"] = resource.NewStringProperty(a.Name + "@demo.iam.gserviceaccount.com")
	}
	return a.Name + "_id", out, nil
}

func (m *mocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	switch a.Token {
	case "gcp:organizations/getClientConfig:getClientConfig":
		return resource.NewPropertyMapFromMap(map[string]any{"accessToken": "tok"}), nil
	case "cloudflare:index/getApiTokenPermissionGroupsList:getApiTokenPermissionGroupsList":
		name := a.Args["name"].StringValue()
		return resource.NewPropertyMapFromMap(map[string]any{
			"results": []any{map[string]any{"id": "pg-" + name, "name": name}},
		}), nil
	}
	return resource.PropertyMap{}, nil
}

func runProgram(t *testing.T) *mocks {
	t.Helper()
	cfg, _ := json.Marshal(map[string]string{
		"sevi:projectId": "demo", "sevi:billingAccount": "AAAAAA-BBBBBB-CCCCCC", "sevi:cloudflareAccountId": "cfacct",
		"sevi:openrouterApiKey": "or-key", "sevi:telegramBotToken": "123:bot",
		"sevi:databaseUrl": "postgresql://sevi:dbpass@ep-1.neon.tech/sevi?sslmode=require",
	})
	secretKeys, _ := json.Marshal([]string{"sevi:openrouterApiKey", "sevi:telegramBotToken", "sevi:databaseUrl"})
	t.Setenv("PULUMI_CONFIG", string(cfg))
	t.Setenv("PULUMI_CONFIG_SECRET_KEYS", string(secretKeys))
	m := &mocks{}
	err := pulumi.RunErr(run, pulumi.WithMocks("sevi", "test", m))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// plain reads a string input that Pulumi may have marked secret.
func plain(v resource.PropertyValue) string {
	if v.IsSecret() {
		v = v.SecretValue().Element
	}
	return v.StringValue()
}

func (m *mocks) all(token string) []mockResource {
	var out []mockResource
	for _, r := range m.resources {
		if r.token == token {
			out = append(out, r)
		}
	}
	return out
}

func (m *mocks) one(t *testing.T, token string) mockResource {
	t.Helper()
	got := m.all(token)
	if len(got) != 1 {
		t.Fatalf("want exactly one %s, got %d", token, len(got))
	}
	return got[0]
}

func TestCloudRunScalesToZeroAndIsPrivate(t *testing.T) {
	m := runProgram(t)
	svc := m.one(t, "gcp:cloudrunv2/service:Service").in.Mappable()
	b, _ := json.Marshal(svc)
	j := string(b)
	for _, want := range []string{`"minInstanceCount":0`, `"maxInstanceCount":1`, `"cpuIdle":false`, `"memory":"1Gi"`, "SEVI_SECRETS_NAME"} {
		if !strings.Contains(j, want) {
			t.Errorf("service missing %s in %s", want, j)
		}
	}
	// Only the gateway may invoke it: never allUsers.
	for _, r := range m.all("gcp:cloudrunv2/serviceIamMember:ServiceIamMember") {
		member := r.in["member"].StringValue()
		if strings.Contains(member, "allUsers") || !strings.HasPrefix(member, "serviceAccount:gateway@") {
			t.Errorf("unexpected invoker %q", member)
		}
	}
	if n := len(m.all("gcp:cloudrunv2/serviceIamMember:ServiceIamMember")); n != 1 {
		t.Errorf("want one invoker, got %d", n)
	}
}

func TestSecretJSONMatchesTheSample(t *testing.T) {
	m := runProgram(t)
	versions := m.all("gcp:secretmanager/secretVersion:SecretVersion")
	if len(versions) != 2 {
		t.Fatalf("want two secret versions, got %d", len(versions))
	}
	sample, err := os.ReadFile("../secret_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	json.Unmarshal(sample, &want)

	var v1, v2 map[string]string
	for _, v := range versions {
		var data map[string]string
		if err := json.Unmarshal([]byte(plain(v.in["secretData"])), &data); err != nil {
			t.Fatalf("%s is not JSON: %v", v.name, err)
		}
		if v.name == "secret-v1" {
			v1 = data
		} else {
			v2 = data
		}
	}
	for k := range want {
		if v2[k] == "" && k != "JINA_API_KEY" {
			t.Errorf("secret v2 has no value for %s (secret_sample.json lists it)", k)
		}
	}
	for k := range v2 {
		if _, ok := want[k]; !ok {
			t.Errorf("secret v2 has %s, which secret_sample.json does not list", k)
		}
	}
	if _, has := v1["TELEGRAM_WEBHOOK_URL"]; has {
		t.Error("v1 must not need the gateway URLs (Cloud Run starts before the gateway exists)")
	}
	if v2["DATABASE_URL"] != "postgresql://sevi:dbpass@ep-1.neon.tech/sevi?sslmode=require" {
		t.Errorf("DATABASE_URL: %s", v2["DATABASE_URL"])
	}
	if v2["R2_ENDPOINT"] != "cfacct.r2.cloudflarestorage.com" || v2["R2_BUCKET"] != "sevi-audio" {
		t.Errorf("R2: %s %s", v2["R2_ENDPOINT"], v2["R2_BUCKET"])
	}
}

func TestGatewaySpecPointsAtCloudRun(t *testing.T) {
	m := runProgram(t)
	cfg := m.one(t, "gcp:apigateway/apiConfig:ApiConfig")
	var contents string
	for _, doc := range cfg.in["openapiDocuments"].ArrayValue() {
		contents = doc.ObjectValue()["document"].ObjectValue()["contents"].StringValue()
	}
	raw, err := base64.StdEncoding.DecodeString(contents)
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	if strings.Contains(spec, "CLOUD_RUN_URL") || !strings.Contains(spec, "https://sevi-abc-uc.a.run.app") {
		t.Error("the placeholder was not replaced by the Cloud Run URL")
	}
	for _, path := range []string{"/telegram:"} {
		if !strings.Contains(spec, path) {
			t.Errorf("spec lacks %s", path)
		}
	}
	if strings.Contains(spec, "/keepalive") || strings.Contains(spec, "\n  /v1") {
		t.Error("the Core API must not be exposed")
	}
}

func TestBudgetAlertsOnFirstPaidCent(t *testing.T) {
	m := runProgram(t)
	b := m.one(t, "gcp:billing/budget:Budget")
	j, _ := json.Marshal(b.in.Mappable())
	for _, want := range []string{`"thresholdPercent":0.01`, `"thresholdPercent":1`, `"units":"1"`, "projects/123456"} {
		if !strings.Contains(string(j), want) {
			t.Errorf("budget missing %s in %s", want, j)
		}
	}
}

func TestSafetyNets(t *testing.T) {
	m := runProgram(t)
	// Registry keeps two images.
	repo := m.one(t, "gcp:artifactregistry/repository:Repository")
	b, _ := json.Marshal(repo.in.Mappable())
	if !strings.Contains(string(b), `"keepCount":2`) {
		t.Errorf("registry cleanup: %s", b)
	}
	// Logs keep seven days.
	logs := m.one(t, "gcp:logging/projectBucketConfig:ProjectBucketConfig")
	if logs.in["retentionDays"].NumberValue() != 7 {
		t.Errorf("log retention: %v", logs.in["retentionDays"])
	}
	// The audio bucket expires old files, and Telegram is pointed at the gateway.
	m.one(t, "cloudflare:index/r2BucketLifecycle:R2BucketLifecycle")
	// Two commands: the schema migration (before Cloud Run) and the webhook registration.
	cmds := m.all("command:local:Command")
	if len(cmds) != 2 {
		t.Fatalf("want 2 commands (db-migrate, set-webhook), got %d", len(cmds))
	}
	for _, c := range cmds {
		if c.name == "db-migrate" {
			if !strings.Contains(c.in["create"].StringValue(), "migrate") || c.in["triggers"].ArrayValue()[0].StringValue() == "" {
				t.Errorf("db-migrate: %v", c.in)
			}
		}
	}
	// The R2 token is limited to the bucket.
	tok := m.one(t, "cloudflare:index/apiToken:ApiToken")
	res := tok.in["policies"].ArrayValue()[0].ObjectValue()["resources"].StringValue()
	if !strings.Contains(res, "cfacct_default_sevi-audio") {
		t.Errorf("R2 token resources: %s", res)
	}
}
