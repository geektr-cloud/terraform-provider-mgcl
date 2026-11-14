package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const providerBlock = `
terraform {
  required_providers {
    mgcl = {
      source = "geektr-cloud/mgcl"
    }
  }
}

provider "mgcl" {}
`

func secretEphemeral(name string) string {
	return `
ephemeral "mgcl_secret" "bootstrap" {
  name = "` + name + `"
}
`
}

// TestE2EValidateDoesNotReadSecret proves the dev_overrides path works without
// `tofu init` and that validate performs no provider API request, with or
// without a provider session in the environment.
func TestE2EValidateDoesNotReadSecret(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+secretEphemeral("bootstrap"))

	t.Run("with session", func(t *testing.T) {
		env := tofuEnv(t, cliConfig, map[string]string{
			"MGCL_PROVIDER_ENDPOINT": api.url(),
			"MGCL_PROVIDER_TOKEN":    "e2e-token",
		})

		res := runTofu(t, tofu, work, env, "validate", "-no-color")
		res.requireSuccess(t, "validate")

		if count := api.requestCount(); count != 0 {
			t.Fatalf("tofu validate made %d provider API requests, want 0", count)
		}
	})

	t.Run("without session", func(t *testing.T) {
		env := tofuEnv(t, cliConfig, nil)

		res := runTofu(t, tofu, work, env, "validate", "-no-color")
		res.requireSuccess(t, "validate")

		if count := api.requestCount(); count != 0 {
			t.Fatalf("tofu validate made %d provider API requests, want 0", count)
		}
	})
}

// applyProbeHCL renders a configuration that reads the secret, asserts it is
// readable during plan, and writes one value to disk at apply time so the test
// can observe what the provider returned.
func applyProbeHCL(secretName, trigger string) string {
	return providerBlock + `
ephemeral "mgcl_secret" "bootstrap" {
  name = "` + secretName + `"
}

check "readable" {
  assert {
    condition     = length(ephemeral.mgcl_secret.bootstrap.values) >= 0
    error_message = "the bootstrap secret must be readable"
  }
}

resource "terraform_data" "probe" {
  triggers_replace = "` + trigger + `"

  provisioner "local-exec" {
    command = "printf '%s' '${ephemeral.mgcl_secret.bootstrap.values["access_key_id"]}' > access_key_id.txt"
  }
}
`
}

// TestE2EPlanAndApplyUseFreshValues covers the full ephemeral lifecycle:
// plan opens the resource, apply opens it again, a later apply sees updated
// values, and no value or token ends up in plan, state or logs.
func TestE2EPlanAndApplyUseFreshValues(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	const token = "e2e-token-value"

	api := newFakeProviderAPI(t, token)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
	})

	secrets := []string{"AKIAE2E", "e2e-s3cr3t-value"}

	writeFile(t, work, "main.tf", applyProbeHCL("bootstrap", "1"))

	planResult := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false", "-out=plan.bin")
	planResult.requireSuccess(t, "plan")

	// dev_overrides must skip provider installation entirely: no `tofu init`
	// and no .terraform/providers tree.
	if _, err := os.Stat(filepath.Join(work, ".terraform", "providers")); err == nil {
		t.Fatal("dev_overrides plan created a provider installation directory; dev_overrides must skip init")
	}

	if count := api.requestCount(); count < 1 {
		t.Fatalf("tofu plan made %d provider API requests, want at least 1", count)
	}

	assertNoSecrets(t, "plan output", planResult.output(), secrets...)
	assertNoToken(t, "plan output", planResult.output(), token)

	assertPlanFileNoLeaks(t, "plan file", filepath.Join(work, "plan.bin"), token, secrets...)

	applyResult := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve", "plan.bin")
	applyResult.requireSuccess(t, "apply")

	if count := api.requestCount(); count < 2 {
		t.Fatalf("plan+apply made %d provider API requests, want at least 2", count)
	}

	assertNoSecrets(t, "apply output", applyResult.output(), secrets...)
	assertNoToken(t, "apply output", applyResult.output(), token)

	requireFileContent(t, filepath.Join(work, "access_key_id.txt"), "AKIAE2E")

	assertNoSecrets(t, "state file", readFileString(t, filepath.Join(work, "terraform.tfstate")), secrets...)
	assertNoToken(t, "state file", readFileString(t, filepath.Join(work, "terraform.tfstate")), token)

	// Update the secret and force a replacement so apply opens the ephemeral
	// resource again and observes the new value.
	api.setValues(map[string]string{"access_key_id": "AKIAE2E-NEW", "secret_access_key": "e2e-s3cr3t-new"})
	writeFile(t, work, "main.tf", applyProbeHCL("bootstrap", "2"))

	refreshedPlan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false", "-out=plan2.bin")
	refreshedPlan.requireSuccess(t, "plan (refresh)")

	refreshedSecrets := []string{"AKIAE2E-NEW", "e2e-s3cr3t-new"}
	assertPlanFileNoLeaks(t, "refreshed plan file", filepath.Join(work, "plan2.bin"), token, refreshedSecrets...)
	assertNoSecrets(t, "refreshed plan output", refreshedPlan.output(), refreshedSecrets...)

	refreshedApply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve", "plan2.bin")
	refreshedApply.requireSuccess(t, "apply (refresh)")

	for _, request := range api.requestList() {
		if request.method != http.MethodPost || request.path != "/api/mgcl/v1/secrets:read" {
			t.Fatalf("provider request = %s %s, want POST /api/mgcl/v1/secrets:read", request.method, request.path)
		}

		if request.token != token {
			t.Fatalf("provider request used bearer token %q, want the injected provider token", request.token)
		}

		if request.name != "bootstrap" {
			t.Fatalf("provider requested secret %q, want bootstrap", request.name)
		}
	}

	requireFileContent(t, filepath.Join(work, "access_key_id.txt"), "AKIAE2E-NEW")

	allSecrets := append([]string{}, secrets...)
	allSecrets = append(allSecrets, "AKIAE2E-NEW", "e2e-s3cr3t-new")

	// TF_LOG=TRACE must not disclose values or the token.
	logPath := filepath.Join(work, "tofu.log")
	logEnv := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    token,
		"TF_LOG":                 "TRACE",
		"TF_LOG_PATH":            logPath,
	})

	logResult := runTofu(t, tofu, work, logEnv, "plan", "-no-color", "-input=false")
	logResult.requireSuccess(t, "plan (TF_LOG=TRACE)")

	logText := readFileString(t, logPath)
	assertNoSecrets(t, "TF_LOG output", logText, allSecrets...)
	assertNoToken(t, "TF_LOG output", logText, token)

	// Lock metadata, if present, must not contain values either.
	lockInfo := filepath.Join(work, ".terraform.tfstate.lock.info")
	if data, err := os.ReadFile(lockInfo); err == nil {
		assertNoSecrets(t, "lock file", string(data), allSecrets...)
	}
}

// TestE2EMisuseAsManagedResource verifies that mgcl_secret is not registered as
// a managed resource.
func TestE2EMisuseAsManagedResource(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}
`)

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    "e2e-token",
	})

	res := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	res.requireFailure(t, "plan")

	output := res.output()
	if !strings.Contains(output, "mgcl_secret") {
		t.Fatalf("failure output does not mention mgcl_secret:\n%s", output)
	}

	if !strings.Contains(output, "Invalid resource type") && !strings.Contains(output, "resource type") {
		t.Fatalf("failure output does not explain the invalid resource type:\n%s", output)
	}

	if count := api.requestCount(); count != 0 {
		t.Fatalf("misuse made %d provider API requests, want 0", count)
	}
}

// TestE2EUnknownName documents how an unknown name behaves at plan time. The
// provider never guesses: either the core defers the ephemeral resource or Open
// fails with a clear diagnostic.
func TestE2EUnknownName(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "name" {
  input = "bootstrap"
}

ephemeral "mgcl_secret" "bootstrap" {
  name = terraform_data.name.output
}

check "readable" {
  assert {
    condition     = length(ephemeral.mgcl_secret.bootstrap.values) >= 0
    error_message = "the bootstrap secret must be readable"
  }
}
`)

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    "e2e-token",
	})

	res := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")

	if res.err != nil {
		output := res.output()
		if !strings.Contains(output, "must be known") {
			t.Fatalf("plan failed without the expected unknown-name diagnostic:\n%s", output)
		}

		if count := api.requestCount(); count != 0 {
			t.Fatalf("unknown name made %d provider API requests, want 0", count)
		}

		return
	}

	// Core deferred the resource; it must not have been read without a name.
	if count := api.requestCount(); count != 0 {
		t.Fatalf("deferred open made %d provider API requests, want 0", count)
	}

	apply := runTofu(t, tofu, work, env, "apply", "-no-color", "-input=false", "-auto-approve")
	apply.requireSuccess(t, "apply")

	if count := api.requestCount(); count < 1 {
		t.Fatalf("apply with a now-known name made %d provider API requests, want at least 1", count)
	}

	for _, request := range api.requestList() {
		if request.name != "bootstrap" {
			t.Fatalf("provider requested secret %q, want bootstrap", request.name)
		}
	}
}

// TestE2EFilesystemMirrorInit covers the init path: an unpacked filesystem
// mirror layout must install the provider so `tofu init` and a real plan work.
func TestE2EFilesystemMirrorInit(t *testing.T) {
	tofu := requireTofu(t)

	binDir := buildProvider(t, providerVersion)
	mirror := installFilesystemMirror(t, binDir, providerVersion)

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "mirror.tfrc", filesystemMirrorConfig(t, mirror))
	writeFile(t, work, "main.tf", providerBlock+secretEphemeral("bootstrap"))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    "e2e-token",
	})

	init := runTofu(t, tofu, work, env, "init", "-no-color", "-input=false")
	init.requireSuccess(t, "init")

	providerDir := filepath.Join(work, ".terraform", "providers", "registry.opentofu.org", "geektr-cloud", "mgcl", providerVersion, runtime.GOOS+"_"+runtime.GOARCH)
	if _, err := os.Stat(providerDir); err != nil {
		t.Fatalf("tofu init did not install the provider from the mirror: %v", err)
	}

	validate := runTofu(t, tofu, work, env, "validate", "-no-color")
	validate.requireSuccess(t, "validate")

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireSuccess(t, "plan")

	if count := api.requestCount(); count < 1 {
		t.Fatalf("plan after init made %d provider API requests, want at least 1", count)
	}

	assertNoSecrets(t, "plan output", plan.output(), "AKIAE2E", "e2e-s3cr3t-value")
	assertNoToken(t, "plan output", plan.output(), "e2e-token")
}

// TestE2EFilesystemMirrorPackedInit covers the packed mirror layout: OpenTofu
// must unpack the provider zip during `tofu init` and execute it.
func TestE2EFilesystemMirrorPackedInit(t *testing.T) {
	tofu := requireTofu(t)

	binDir := buildProvider(t, providerVersion)
	mirror := installPackedFilesystemMirror(t, binDir, providerVersion)

	api := newFakeProviderAPI(t, "e2e-token")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "mirror.tfrc", filesystemMirrorConfig(t, mirror))
	writeFile(t, work, "main.tf", providerBlock+secretEphemeral("bootstrap"))

	env := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_PROVIDER_ENDPOINT": api.url(),
		"MGCL_PROVIDER_TOKEN":    "e2e-token",
	})

	init := runTofu(t, tofu, work, env, "init", "-no-color", "-input=false")
	init.requireSuccess(t, "init")

	providerDir := filepath.Join(work, ".terraform", "providers", "registry.opentofu.org", "geektr-cloud", "mgcl", providerVersion, runtime.GOOS+"_"+runtime.GOARCH)
	entries, err := os.ReadDir(providerDir)
	if err != nil {
		t.Fatalf("tofu init did not install the packed provider: %v", err)
	}

	installed := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "terraform-provider-mgcl") {
			installed = true
		}
	}

	if !installed {
		t.Fatalf("packed provider binary not found under %s", providerDir)
	}

	validate := runTofu(t, tofu, work, env, "validate", "-no-color")
	validate.requireSuccess(t, "validate")

	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireSuccess(t, "plan")

	if count := api.requestCount(); count < 1 {
		t.Fatalf("plan after packed-mirror init made %d provider API requests, want at least 1", count)
	}

	assertNoSecrets(t, "plan output", plan.output(), "AKIAE2E", "e2e-s3cr3t-value")
	assertNoToken(t, "plan output", plan.output(), "e2e-token")
}

func readFileString(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(data)
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()

	if got := readFileString(t, path); got != want {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}
