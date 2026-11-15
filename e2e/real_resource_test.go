package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRealSecretKVResourceLifecycle creates two KVs in one Secret through the
// managed resource, updates and renames them, then destroys them. The Secret
// remains, empty, because the provider API deletes KVs idempotently.
func TestRealSecretKVResourceLifecycle(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

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

	capture1 := filepath.Join(work, "env1.txt")
	env1 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	apply := runMgcl(t, mgcl, work, env1, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (two KVs in one Secret)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "apply output", apply.output(), token1)

	secretEnv := tofuEnv(t, "", nil)

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", "app-config/alpha", "--db", db, "--encryption-key-file", key); got != "one" {
		t.Fatalf("app-config/alpha = %q, want one", got)
	}

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", "app-config/beta", "--db", db, "--encryption-key-file", key); got != "two" {
		t.Fatalf("app-config/beta = %q, want two", got)
	}

	// The managed resource value is deliberately persisted in state.
	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist resource value %q:\n%s", want, state)
		}
	}

	assertNoToken(t, "state file", state, token1)

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

	capture2 := filepath.Join(work, "env2.txt")
	env2 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	update := runMgcl(t, mgcl, work, env2, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	update.requireSuccess(t, "mgcl run -- tofu apply (update and key replacement)")

	_, token2 := readCapture(t, capture2)
	assertNoToken(t, "update output", update.output(), token1)
	assertNoToken(t, "update output", update.output(), token2)

	missing := runMgcl(t, mgcl, t.TempDir(), secretEnv, "secret", "get", "app-config/alpha", "--db", db, "--encryption-key-file", key)
	missing.requireFailure(t, "secret get of a replaced key")

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", "app-config/alpha_renamed", "--db", db, "--encryption-key-file", key); got != "one-updated" {
		t.Fatalf("app-config/alpha_renamed = %q, want one-updated", got)
	}

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", "app-config/beta", "--db", db, "--encryption-key-file", key); got != "two-updated" {
		t.Fatalf("app-config/beta = %q, want two-updated", got)
	}

	capture3 := filepath.Join(work, "env3.txt")
	env3 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture3})

	destroy := runMgcl(t, mgcl, work, env3, mgclRunArgs(db, key,
		captureCommand(capture3, tofu, "destroy", "-no-color", "-input=false", "-auto-approve")...)...)
	destroy.requireSuccess(t, "mgcl run -- tofu destroy (delete KVs)")

	_, token3 := readCapture(t, capture3)
	assertNoToken(t, "destroy output", destroy.output(), token3)

	show := secretCLI(t, mgcl, secretEnv, "secret", "show", "app-config", "-o", "json", "--db", db)
	if !strings.Contains(show, `"keys": []`) && !strings.Contains(show, `"keys":[]`) {
		t.Fatalf("Secret should remain empty after KV deletion:\n%s", show)
	}
}

// TestRealResourceWrittenKVsReadElsewhere proves KVs written by the managed
// resource are visible to the public function and to mgcl_secret in a separate
// OpenTofu project.
func TestRealResourceWrittenKVsReadElsewhere(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	const (
		secretName   = "written-config"
		publicRegion = "eu-west-1"
		publicEnv    = "staging"
		ephemeralVal = "EPHEMERAL-WRITTEN"
	)

	writeWork := t.TempDir()
	writeConfig := writeFile(t, writeWork, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, writeWork, "main.tf", providerBlock+`
resource "mgcl_secret_kv" "region" {
  key            = "`+secretName+`/region"
  value          = "`+publicRegion+`"
  ephemeral_mark = false
}

resource "mgcl_secret_kv" "env" {
  key            = "`+secretName+`/env"
  value          = "`+publicEnv+`"
  ephemeral_mark = false
}

resource "mgcl_secret_kv" "token" {
  key   = "`+secretName+`/token"
  value = "`+ephemeralVal+`"
}
`)

	capture1 := filepath.Join(writeWork, "env1.txt")
	writeEnv := tofuEnv(t, writeConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	apply := runMgcl(t, mgcl, writeWork, writeEnv, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (resource-written KVs)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "write apply output", apply.output(), token1)

	// Separate project: public function single and wildcard.
	readWork := t.TempDir()
	readConfig := writeFile(t, t.TempDir(), "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, readWork, "main.tf", providerBlock+`
resource "terraform_data" "single" {
  input = provider::mgcl::secret_value("`+secretName+`/region")
}

resource "terraform_data" "wildcard" {
  input = provider::mgcl::secret_value("`+secretName+`/*")
}
`)

	capture2 := filepath.Join(readWork, "env2.txt")
	readEnv := tofuEnv(t, readConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	readApply := runMgcl(t, mgcl, readWork, readEnv, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	readApply.requireSuccess(t, "mgcl run -- tofu apply (read resource-written KVs)")

	_, token2 := readCapture(t, capture2)

	readState := readFileString(t, filepath.Join(readWork, "terraform.tfstate"))
	for _, want := range []string{publicRegion, publicEnv, `"region"`, `"env"`} {
		if !strings.Contains(readState, want) {
			t.Fatalf("state does not contain public function result %q:\n%s", want, readState)
		}
	}

	if strings.Contains(readState, ephemeralVal) {
		t.Fatalf("public function returned the ephemeral KV:\n%s", readState)
	}

	assertNoToken(t, "read state file", readState, token1)
	assertNoToken(t, "read state file", readState, token2)
	assertNoSecrets(t, "read apply output", readApply.output(), ephemeralVal)

	// Separate project: mgcl_secret returns every KV, including the ephemeral
	// one, without persisting values.
	ephWork := t.TempDir()
	ephConfig := writeFile(t, t.TempDir(), "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, ephWork, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "mixed" {
  name = "`+secretName+`"
}

resource "terraform_data" "probe" {
  provisioner "local-exec" {
    command = "printf '%s|%s|%s' '${ephemeral.mgcl_secret.mixed.values["region"]}' '${ephemeral.mgcl_secret.mixed.values["env"]}' '${ephemeral.mgcl_secret.mixed.values["token"]}' > values.txt"
  }
}
`)

	capture3 := filepath.Join(ephWork, "env3.txt")
	ephEnv := tofuEnv(t, ephConfig, map[string]string{"MGCL_E2E_CAPTURE": capture3})

	ephApply := runMgcl(t, mgcl, ephWork, ephEnv, mgclRunArgs(db, key,
		captureCommand(capture3, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	ephApply.requireSuccess(t, "mgcl run -- tofu apply (ephemeral mixed Secret)")

	_, token3 := readCapture(t, capture3)
	requireFileContent(t, filepath.Join(ephWork, "values.txt"), publicRegion+"|"+publicEnv+"|"+ephemeralVal)

	ephState := readFileString(t, filepath.Join(ephWork, "terraform.tfstate"))
	assertNoSecrets(t, "ephemeral state file", ephState, publicRegion, publicEnv, ephemeralVal)
	assertNoToken(t, "ephemeral state file", ephState, token3)
	assertNoSecrets(t, "ephemeral apply output", ephApply.output(), publicRegion, publicEnv, ephemeralVal)
}
