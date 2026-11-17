package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

// TestE2EManagedSecretLifecycle creates a hierarchical Secret with several KVs
// in one apply, updates values and marks, removes one key and adds another,
// then destroys the resource. The Secret and any unmanaged KV must survive.
func TestE2EManagedSecretLifecycle(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-secret-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("aws-605592676484-cn/scarborough",
		map[string]string{"untracked": "keep-me"},
		map[string]bool{"untracked": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "scarborough_cred" {
  name = "aws-605592676484-cn/scarborough"
  contents = {
    account_id = { value = "1234", ephemeral_mark = false }
    region     = { value = "cn-north-1", ephemeral_mark = false }
    secret_key = { value = "s3cr3t-value" }
  }
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply creating a managed Secret")

	values, marks, ok := api.secretValues("aws-605592676484-cn/scarborough")
	if !ok {
		t.Fatal("managed Secret was not created")
	}

	if len(values) != 4 {
		t.Fatalf("stored values = %v, want three managed KVs plus the untracked KV", values)
	}

	if values["account_id"] != "1234" || marks["account_id"] != false {
		t.Fatalf("account_id = %q/%v, want 1234/public", values["account_id"], marks["account_id"])
	}

	if values["region"] != "cn-north-1" || marks["region"] != false {
		t.Fatalf("region = %q/%v, want cn-north-1/public", values["region"], marks["region"])
	}

	if values["secret_key"] != "s3cr3t-value" || marks["secret_key"] != true {
		t.Fatalf("secret_key = %q/%v, want s3cr3t-value/ephemeral by default", values["secret_key"], marks["secret_key"])
	}

	if values["untracked"] != "keep-me" {
		t.Fatalf("untracked KV = %q, want keep-me", values["untracked"])
	}

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{"1234", "cn-north-1", "s3cr3t-value"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist managed value %q", want)
		}
	}

	assertNoToken(t, "apply output", apply.output(), token)
	assertNoToken(t, "state file", state, token)

	// A second apply without configuration changes must be a no-op.
	second := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	second.requireSuccess(t, "second apply")

	if !strings.Contains(second.output(), "0 added, 0 changed, 0 destroyed") {
		t.Fatalf("second apply was not a no-op:\n%s", second.output())
	}

	// Update in place, remove region, and add session_token.
	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "scarborough_cred" {
  name = "aws-605592676484-cn/scarborough"
  contents = {
    account_id    = { value = "5678", ephemeral_mark = true }
    secret_key    = { value = "rotated", ephemeral_mark = false }
    session_token = { value = "session", ephemeral_mark = false }
  }
}
`)

	update := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	update.requireSuccess(t, "apply updating the managed Secret")

	values, marks, ok = api.secretValues("aws-605592676484-cn/scarborough")
	if !ok {
		t.Fatal("Secret disappeared after update")
	}

	if _, exists := values["region"]; exists {
		t.Fatalf("removed key region still exists: %v", values)
	}

	if values["account_id"] != "5678" || marks["account_id"] != true {
		t.Fatalf("account_id = %q/%v, want 5678/ephemeral", values["account_id"], marks["account_id"])
	}

	if values["secret_key"] != "rotated" || marks["secret_key"] != false {
		t.Fatalf("secret_key = %q/%v, want rotated/public", values["secret_key"], marks["secret_key"])
	}

	if values["session_token"] != "session" || marks["session_token"] != false {
		t.Fatalf("session_token = %q/%v, want session/public", values["session_token"], marks["session_token"])
	}

	if values["untracked"] != "keep-me" {
		t.Fatalf("update touched the untracked KV: %v", values)
	}

	var batchRequests int

	for _, call := range api.requestList() {
		if call.path != providerapi.SecretKVsWriteBatchPath {
			continue
		}

		batchRequests++

		var payload providerapi.WriteSecretKVsRequest
		if err := json.Unmarshal(call.body, &payload); err != nil {
			t.Fatalf("decoding batch request: %v", err)
		}

		for _, key := range payload.DeleteKeys {
			if key == "region" {
				goto foundDelete
			}
		}
	}

	t.Fatal("no update request deleted the removed key")

foundDelete:
	if batchRequests < 2 {
		t.Fatalf("batch requests = %d, want create and update", batchRequests)
	}

	// Destroy only the KVs owned by the resource. The Secret and the untracked
	// KV must survive.
	destroy := runTofu(t, tofu, work, env, "destroy", "-no-color", "-input=false", "-auto-approve")
	destroy.requireSuccess(t, "destroy the managed Secret")

	values, _, ok = api.secretValues("aws-605592676484-cn/scarborough")
	if !ok {
		t.Fatal("destroy removed the Secret itself")
	}

	if len(values) != 1 || values["untracked"] != "keep-me" {
		t.Fatalf("values after destroy = %v, want only the untracked KV", values)
	}
}

// TestE2EManagedSecretDeleteMissingSecretDoesNotCreate proves destroy is
// idempotent and never creates a Secret.
func TestE2EManagedSecretDeleteMissingSecretDoesNotCreate(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-delete-token"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "cred" {
  name = "gone/cred"
  contents = {
    secret_key = { value = "v" }
  }
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply")

	api.mu.Lock()
	delete(api.secrets, "gone/cred")
	api.mu.Unlock()

	destroy := runTofu(t, tofu, work, env, "destroy", "-no-color", "-input=false", "-auto-approve")
	destroy.requireSuccess(t, "destroy after the Secret disappeared")

	if values, _, ok := api.secretValues("gone/cred"); ok {
		t.Fatalf("destroy recreated the Secret: %v", values)
	}
}

// TestE2EManagedSecretSameNameAsEphemeral declares both categories with the
// same name. Same names are legal because they are different resource kinds.
func TestE2EManagedSecretSameNameAsEphemeral(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-same-name-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("shared/cred",
		map[string]string{"seed": "seed-value"},
		map[string]bool{"seed": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "shared" {
  name = "shared/cred"
  contents = {
    written = { value = "managed-value" }
  }
}

ephemeral "mgcl_secret" "shared" {
  name = "shared/cred"
}

check "ephemeral_read" {
  assert {
    condition     = ephemeral.mgcl_secret.shared.values["seed"] == "seed-value"
    error_message = "the ephemeral resource must read the same-named Secret"
  }
}
`)

	validate := runTofu(t, tofu, work, env, "validate", "-no-color")
	validate.requireSuccess(t, "validate with a same-named managed and ephemeral resource")

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireSuccess(t, "plan with a same-named managed and ephemeral resource")

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply with a same-named managed and ephemeral resource")

	values, marks, ok := api.secretValues("shared/cred")
	if !ok || values["written"] != "managed-value" || !marks["written"] {
		t.Fatalf("managed write = %q/%v, want managed-value/ephemeral", values["written"], marks["written"])
	}

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	if strings.Contains(state, "seed-value") {
		t.Fatal("ephemeral value leaked into state")
	}

	assertNoToken(t, "apply output", apply.output(), token)
}

// TestE2EManagedSecretFromPublicFunction follows the user example: public KVs
// are read by provider::mgcl::secret_value and written into a managed Secret
// with per-entry marks.
func TestE2EManagedSecretFromPublicFunction(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-function-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("aws-605592676484-cn/source",
		map[string]string{"account_id": "1234", "region": "cn-north-1"},
		map[string]bool{"account_id": false, "region": false},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "target" {
  name = "aws-605592676484-cn/target"
  contents = {
    account_id = {
      value          = provider::mgcl::secret_value("aws-605592676484-cn/source/account_id")
      ephemeral_mark = false
    }
    region = {
      value          = provider::mgcl::secret_value("aws-605592676484-cn/source/region")
      ephemeral_mark = false
    }
    secret_key = {
      value = "composed-in-hcl"
    }
  }
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply a managed Secret fed by the public function")

	values, marks, ok := api.secretValues("aws-605592676484-cn/target")
	if !ok {
		t.Fatal("target Secret was not created")
	}

	if values["account_id"] != "1234" || marks["account_id"] != false {
		t.Fatalf("account_id = %q/%v, want 1234/public", values["account_id"], marks["account_id"])
	}

	if values["region"] != "cn-north-1" || marks["region"] != false {
		t.Fatalf("region = %q/%v, want cn-north-1/public", values["region"], marks["region"])
	}

	if values["secret_key"] != "composed-in-hcl" || marks["secret_key"] != true {
		t.Fatalf("secret_key = %q/%v, want composed-in-hcl/ephemeral", values["secret_key"], marks["secret_key"])
	}

	var publicReads int

	for _, call := range api.requestList() {
		if call.path == providerapi.SecretsReadPath && call.access == providerapi.SecretAccessPublic {
			publicReads++
		}
	}

	if publicReads == 0 {
		t.Fatal("the public function never read the source Secret")
	}
}

// TestE2EManagedSecretIndependentReadProject reads a managed Secret from a
// separate OpenTofu project using both the ephemeral resource and the public
// function.
func TestE2EManagedSecretIndependentReadProject(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-read-token"

	api := newFakeProviderAPI(t, token)

	writeWork := t.TempDir()
	writeConfig := writeFile(t, writeWork, "dev.tfrc", devOverridesConfig(t, binDir))

	writeEnv := tofuEnv(t, writeConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, writeWork, "main.tf", providerBlock+`
resource "mgcl_secret" "target" {
  name = "shared/target"
  contents = {
    account_id = { value = "1234", ephemeral_mark = false }
    secret_key = { value = "s3cr3t-value", ephemeral_mark = true }
  }
}
`)

	apply := runTofu(t, tofu, writeWork, writeEnv, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply in the write project")

	readWork := t.TempDir()
	readConfig := writeFile(t, readWork, "dev.tfrc", devOverridesConfig(t, binDir))

	readEnv := tofuEnv(t, readConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, readWork, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "target" {
  name = "shared/target"
}

resource "terraform_data" "public" {
  input = provider::mgcl::secret_value("shared/target/account_id")
}

check "ephemeral_read" {
  assert {
    condition     = ephemeral.mgcl_secret.target.values["secret_key"] == "s3cr3t-value"
    error_message = "the independent project must read the ephemeral KV"
  }
}
`)

	readApply := runTofu(t, tofu, readWork, readEnv, "apply", "-no-color", "-input=false", "-auto-approve")
	readApply.requireSuccess(t, "apply in the independent read project")

	readState := readFileString(t, filepath.Join(readWork, "terraform.tfstate"))
	if strings.Contains(readState, "s3cr3t-value") {
		t.Fatal("the ephemeral KV leaked into the read project's state")
	}

	if !strings.Contains(readState, "1234") {
		t.Fatal("the public KV was not persisted by terraform_data as expected")
	}

	assertNoSecrets(t, "read apply output", readApply.output(), "s3cr3t-value")
	assertNoToken(t, "read apply output", readApply.output(), token)
}

// TestE2EManagedSecretPathEscapeStableNames applies a dotted name and
// mixed-case, dotted KV keys that are valid because url.PathEscape leaves each
// segment unchanged, even though they fall outside the old lowercase slug grammar.
func TestE2EManagedSecretPathEscapeStableNames(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-path-escape-token"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "dotted" {
  name = "net.sxxfuture.infra"
  contents = {
    "Region.Name" = { value = "us-east-1", ephemeral_mark = false }
    "API-Key"     = { value = "s3cr3t-value" }
  }
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply creating a PathEscape-stable managed Secret")

	values, marks, ok := api.secretValues("net.sxxfuture.infra")
	if !ok {
		t.Fatal("managed Secret with a dotted name was not created")
	}

	if len(values) != 2 {
		t.Fatalf("stored values = %v, want two mixed-case dotted KVs", values)
	}

	if values["Region.Name"] != "us-east-1" || marks["Region.Name"] != false {
		t.Fatalf("Region.Name = %q/%v, want us-east-1/public", values["Region.Name"], marks["Region.Name"])
	}

	if values["API-Key"] != "s3cr3t-value" || marks["API-Key"] != true {
		t.Fatalf("API-Key = %q/%v, want s3cr3t-value/ephemeral", values["API-Key"], marks["API-Key"])
	}

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	if !strings.Contains(state, "net.sxxfuture.infra") || !strings.Contains(state, "Region.Name") {
		t.Fatal("state does not persist the PathEscape-stable name and key")
	}

	assertNoToken(t, "apply output", apply.output(), token)
	assertNoToken(t, "state file", state, token)
}

// TestE2EManagedSecretValidationFailsBeforeRequests covers config-time
// validation for hierarchical names and bare KV keys.
func TestE2EManagedSecretValidationFailsBeforeRequests(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-managed-validation-token"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	cases := []struct {
		name string
		hcl  string
		want string
	}{
		{
			name: "invalid name",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "Team cred"
  contents = {
    key = { value = "v" }
  }
}
`,
			want: "Invalid secret name",
		},
		{
			name: "name with percent",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team%cred"
  contents = {
    key = { value = "v" }
  }
}
`,
			want: "Invalid secret name",
		},
		{
			name: "name with empty segment",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team//cred"
  contents = {
    key = { value = "v" }
  }
}
`,
			want: "Invalid secret name",
		},
		{
			name: "name with wildcard segment",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team/*"
  contents = {
    key = { value = "v" }
  }
}
`,
			want: "Invalid secret name",
		},
		{
			name: "key with slash",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team/cred"
  contents = {
    "nested/key" = { value = "v" }
  }
}
`,
			want: "Invalid secret key",
		},
		{
			name: "key with unicode",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team/cred"
  contents = {
    "团队" = { value = "v" }
  }
}
`,
			want: "Invalid secret key",
		},
		{
			name: "null value",
			hcl: `
resource "mgcl_secret" "bad" {
  name = "team/cred"
  contents = {
    key = { value = null }
  }
}
`,
			want: "Secret value must not be null",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, work, "main.tf", providerBlock+tc.hcl)

			validate := runTofu(t, tofu, work, env, "validate", "-no-color")
			validate.requireFailure(t, "validate")

			if !strings.Contains(strings.Join(strings.Fields(validate.output()), " "), tc.want) {
				t.Fatalf("failure does not contain %q:\n%s", tc.want, validate.output())
			}

			if count := api.requestCount(); count != 0 {
				t.Fatalf("invalid configuration made %d provider API requests, want 0", count)
			}
		})
	}

	// A null entry object must be reported by OpenTofu core, never as a
	// framework "provider bug" conversion error, and must not reach the API.
	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "bad" {
  name = "team/cred"
  contents = {
    key = null
  }
}
`)

	nullPlan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	nullPlan.requireFailure(t, "plan with a null contents entry")

	if output := nullPlan.output(); strings.Contains(output, "provider bug") || strings.Contains(output, "Value Conversion Error") {
		t.Fatalf("null contents entry surfaced a framework conversion error:\n%s", output)
	}

	if count := api.requestCount(); count != 0 {
		t.Fatalf("null contents entry made %d provider API requests, want 0", count)
	}

	// An empty map is valid and ensures an empty Secret.
	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "empty" {
  name = "team/empty"
  contents = {}
}
`)

	validate := runTofu(t, tofu, work, env, "validate", "-no-color")
	validate.requireSuccess(t, "validate an empty contents map")

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply an empty contents map")

	if values, _, ok := api.secretValues("team/empty"); !ok || len(values) != 0 {
		t.Fatalf("empty contents = %v exists=%v, want an empty Secret", values, ok)
	}
}
