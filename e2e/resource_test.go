package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

// TestE2ESecretKVResourceLifecycle creates two KVs in one Secret during a
// single apply, updates them in place, replaces a renamed key, then deletes all
// KVs while leaving an empty Secret.
func TestE2ESecretKVResourceLifecycle(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-resource-token"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret_kv" "alpha" {
  key            = "app-config/alpha"
  value          = "one"
  ephemeral_mark = false
}

resource "mgcl_secret_kv" "beta" {
  key   = "app-config/beta"
  value = "two"
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply creating two KVs in one Secret")

	values, marks, ok := api.secretValues("app-config")
	if !ok || len(values) != 2 {
		t.Fatalf("stored values = %v, want two KVs in one Secret", values)
	}

	if values["alpha"] != "one" || marks["alpha"] != false {
		t.Fatalf("alpha = %q/%v, want one/public", values["alpha"], marks["alpha"])
	}

	if values["beta"] != "two" || marks["beta"] != true {
		t.Fatalf("beta = %q/%v, want two/ephemeral from the default mark", values["beta"], marks["beta"])
	}

	// Resource values are persisted in state by design; only the token must
	// never appear.
	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist the resource value %q:\n%s", want, state)
		}
	}

	assertNoToken(t, "apply output", apply.output(), token)
	assertNoToken(t, "state file", state, token)

	// Update value and mark in place, and replace the key on alpha.
	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret_kv" "alpha" {
  key            = "app-config/alpha_renamed"
  value          = "one-updated"
  ephemeral_mark = true
}

resource "mgcl_secret_kv" "beta" {
  key            = "app-config/beta"
  value          = "two-updated"
  ephemeral_mark = false
}
`)

	update := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	update.requireSuccess(t, "apply updating and replacing KVs")

	values, marks, ok = api.secretValues("app-config")
	if !ok {
		t.Fatal("Secret disappeared after update")
	}

	if _, exists := values["alpha"]; exists {
		t.Fatalf("replaced key alpha still exists: %v", values)
	}

	if values["alpha_renamed"] != "one-updated" || marks["alpha_renamed"] != true {
		t.Fatalf("alpha_renamed = %q/%v, want one-updated/ephemeral", values["alpha_renamed"], marks["alpha_renamed"])
	}

	if values["beta"] != "two-updated" || marks["beta"] != false {
		t.Fatalf("beta = %q/%v, want two-updated/public", values["beta"], marks["beta"])
	}

	// Delete every managed KV. The Secret itself remains, empty, because
	// DeleteSecretKV is idempotent and never removes the Secret.
	destroy := runTofu(t, tofu, work, env, "destroy", "-no-color", "-input=false", "-auto-approve")
	destroy.requireSuccess(t, "destroy all KVs")

	values, _, ok = api.secretValues("app-config")
	if !ok {
		t.Fatal("destroy removed the Secret; only the KV should be deleted")
	}

	if len(values) != 0 {
		t.Fatalf("values after destroy = %v, want an empty Secret", values)
	}

	var writes, deletes int

	for _, call := range api.requestList() {
		switch call.path {
		case providerapi.SecretKVsWritePath:
			writes++
		case providerapi.SecretKVsDeletePath:
			deletes++
		case providerapi.SecretsReadPath:
			if call.access != "ephemeral" {
				t.Fatalf("resource read access = %q, want ephemeral", call.access)
			}
		}
	}

	if writes == 0 || deletes == 0 {
		t.Fatalf("provider API writes=%d deletes=%d, want both", writes, deletes)
	}
}

// TestE2ESecretKVResourceDrift proves Read synchronizes out-of-band changes.
func TestE2ESecretKVResourceDrift(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-resource-token"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret_kv" "kv" {
  key            = "drift/value"
  value          = "configured"
  ephemeral_mark = true
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply creating drift/value")

	api.setSecret("drift", map[string]string{"value": "out-of-band"}, map[string]bool{"value": false})

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false", "-detailed-exitcode")
	// -detailed-exitcode returns 2 when a diff exists.
	if plan.err == nil {
		t.Fatal("plan did not detect drift after the KV changed out of band")
	}

	reapply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	reapply.requireSuccess(t, "apply restoring configured value")

	values, marks, ok := api.secretValues("drift")
	if !ok || values["value"] != "configured" || marks["value"] != true {
		t.Fatalf("value/mark = %q/%v, want configured/ephemeral", values["value"], marks["value"])
	}
}

// TestE2ESecretKVResourceInvalidKey makes no provider request.
func TestE2ESecretKVResourceInvalidKey(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    "e2e-token",
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret_kv" "invalid" {
  key   = "not-a-reference"
  value = "v"
}
`)

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireFailure(t, "plan with an invalid resource key")

	if output := plan.output(); !strings.Contains(strings.Join(strings.Fields(output), " "), "Invalid secret KV reference") {
		t.Fatalf("failure does not explain the invalid key:\n%s", output)
	}

	if count := api.requestCount(); count != 0 {
		t.Fatalf("invalid resource key made %d provider API requests, want 0", count)
	}
}
