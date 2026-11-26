package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestE2ESecretValueFunctionSingleAndWildcard covers the dynamic return:
// <secret>/<key> returns a string, <secret>/* returns a map of public KVs, and
// both results are deliberately persisted in state.
func TestE2ESecretValueFunctionSingleAndWildcard(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-function-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("app-config",
		map[string]string{
			"region": "us-east-1",
			"env":    "prod",
			"token":  "EPHEMERAL-SECRET-VALUE",
		},
		map[string]bool{"region": false, "env": false, "token": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "single" {
  input = provider::mgcl::secret_value("app-config/region")
}

resource "terraform_data" "wildcard" {
  input = provider::mgcl::secret_value("app-config/*")
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply with public provider function")

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{"us-east-1", `"region"`, `"env"`, "prod"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not contain public function result %q:\n%s", want, state)
		}
	}

	if strings.Contains(state, "EPHEMERAL-SECRET-VALUE") {
		t.Fatal("state contains an ephemeral KV that the public function must not return")
	}

	assertNoToken(t, "apply output", apply.output(), token)
	assertNoToken(t, "state file", state, token)
	assertNoSecrets(t, "apply output", apply.output(), "EPHEMERAL-SECRET-VALUE")
	assertNoSecrets(t, "state file", state, "EPHEMERAL-SECRET-VALUE")

	calls := api.requestList()
	if len(calls) == 0 {
		t.Fatal("plan/apply made no provider API request")
	}

	for _, call := range calls {
		if call.path != "/api/mgcl/v1/secrets:read" {
			t.Fatalf("unexpected provider request path %q", call.path)
		}

		if call.access != "public" {
			t.Fatalf("access = %q, want public", call.access)
		}

		if call.name != "app-config" {
			t.Fatalf("requested secret = %q, want app-config", call.name)
		}
	}
}

const ephemeralValueFunction = "provider::mgcl::i_just_fucking_wanna_read_ephemeral"

// TestE2ESecretValueEphemeralFunctionSingleAndWildcard covers the all-KV
// function through a real OpenTofu binary: <secret>/<key> returns a string,
// <secret>/* returns every KV, access is always "ephemeral", and both results
// are deliberately persisted in state because this function has no OpenTofu
// ephemeral semantics.
func TestE2ESecretValueEphemeralFunctionSingleAndWildcard(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-ephemeral-function-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("mixed",
		map[string]string{
			"public_kv":    "PUBLIC-VALUE",
			"ephemeral_kv": "EPHEMERAL-VALUE",
		},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "single" {
  input = `+ephemeralValueFunction+`("mixed/ephemeral_kv")
}

resource "terraform_data" "wildcard" {
  input = `+ephemeralValueFunction+`("mixed/*")
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply with all-KV provider function")

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{"PUBLIC-VALUE", "EPHEMERAL-VALUE", `"public_kv"`, `"ephemeral_kv"`} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist all-KV function result %q:\n%s", want, state)
		}
	}

	assertNoToken(t, "apply output", apply.output(), token)
	assertNoToken(t, "state file", state, token)

	calls := api.requestList()
	if len(calls) == 0 {
		t.Fatal("plan/apply made no provider API request")
	}

	for _, call := range calls {
		if call.path != "/api/mgcl/v1/secrets:read" {
			t.Fatalf("unexpected provider request path %q", call.path)
		}

		if call.access != "ephemeral" {
			t.Fatalf("access = %q, want ephemeral", call.access)
		}

		if call.name != "mixed" {
			t.Fatalf("requested secret = %q, want mixed", call.name)
		}
	}
}

// TestE2ESecretValueFunctionRejectsEphemeralKV proves an ephemeral KV is not
// part of the public set and never appears in the failure output.
func TestE2ESecretValueFunctionRejectsEphemeralKV(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-function-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("app-config",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "ephemeral" {
  input = provider::mgcl::secret_value("app-config/ephemeral_kv")
}
`)

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireFailure(t, "plan reading an ephemeral KV through the public function")

	output := plan.output()
	normalized := strings.Join(strings.Fields(output), " ")
	if !strings.Contains(normalized, "was not found among the public KVs") {
		t.Fatalf("failure does not explain the public-only set:\n%s", output)
	}

	assertNoToken(t, "failure output", output, token)
	assertNoSecrets(t, "failure output", output, "EPHEMERAL-VALUE", "PUBLIC-VALUE")

	calls := api.requestList()
	if len(calls) != 1 {
		t.Fatalf("provider API requests = %d, want 1 public read", len(calls))
	}

	if calls[0].access != "public" {
		t.Fatalf("access = %q, want public", calls[0].access)
	}
}

// TestE2ESecretValueFunctionInvalidReference makes zero requests.
func TestE2ESecretValueFunctionInvalidReference(t *testing.T) {
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
resource "terraform_data" "invalid" {
  input = provider::mgcl::secret_value("a//b")
}
`)

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireFailure(t, "plan with an invalid function reference")

	if output := plan.output(); !strings.Contains(strings.Join(strings.Fields(output), " "), "Invalid secret reference") {
		t.Fatalf("failure does not explain the invalid reference:\n%s", output)
	}

	if count := api.requestCount(); count != 0 {
		t.Fatalf("invalid reference made %d provider API requests, want 0", count)
	}
}

// TestE2EEphemeralResourceReturnsAllKVs proves mgcl_secret reads public and
// ephemeral KVs together without persisting them.
func TestE2EEphemeralResourceReturnsAllKVs(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-ephemeral-token"

	api := newFakeProviderAPI(t, token)
	api.setSecret("mixed",
		map[string]string{"public_kv": "PUBLIC-VALUE", "ephemeral_kv": "EPHEMERAL-VALUE"},
		map[string]bool{"public_kv": false, "ephemeral_kv": true},
	)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	writeFile(t, work, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "mixed" {
  name = "mixed"
}

resource "terraform_data" "probe" {
  provisioner "local-exec" {
    command = "printf '%s|%s' '${ephemeral.mgcl_secret.mixed.values["public_kv"]}' '${ephemeral.mgcl_secret.mixed.values["ephemeral_kv"]}' > values.txt"
  }
}
`)

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply with a mixed Secret")

	requireFileContent(t, filepath.Join(work, "values.txt"), "PUBLIC-VALUE|EPHEMERAL-VALUE")

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	assertNoSecrets(t, "state file", state, "PUBLIC-VALUE", "EPHEMERAL-VALUE")
	assertNoToken(t, "state file", state, token)
	assertNoSecrets(t, "apply output", apply.output(), "PUBLIC-VALUE", "EPHEMERAL-VALUE")

	calls := api.requestList()
	if len(calls) < 1 {
		t.Fatalf("provider API requests = %d, want at least one ephemeral read", len(calls))
	}

	for _, call := range calls {
		if call.access != "ephemeral" {
			t.Fatalf("access = %q, want ephemeral", call.access)
		}

		if call.name != "mixed" {
			t.Fatalf("name = %q, want mixed", call.name)
		}
	}
}
