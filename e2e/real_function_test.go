package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRealPublicSecretFunction drives the public provider function through the
// real mgcl CLI, provider API and OpenTofu. Public results are persisted;
// ephemeral KVs stay invisible to the function but remain readable by
// mgcl_secret.
func TestRealPublicSecretFunction(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	secretEnv := tofuEnv(t, "", nil)

	const (
		secretName    = "app-config"
		publicRegion  = "us-east-1"
		publicEnvName = "prod"
		ephemeralVal  = "EPHEMERAL-REAL-VALUE"
	)

	secretCLI(t, mgcl, secretEnv,
		"secret", "create", secretName, "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", secretName+"/region", publicRegion, "--public", "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", secretName+"/env", publicEnvName, "--public", "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", secretName+"/token", ephemeralVal, "--db", db, "--encryption-key-file", key)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "single" {
  input = provider::mgcl::secret_value("`+secretName+`/region")
}

resource "terraform_data" "wildcard" {
  input = provider::mgcl::secret_value("`+secretName+`/*")
}
`)

	capture1 := filepath.Join(work, "env1.txt")
	planEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	plan := runMgcl(t, mgcl, work, planEnv, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "plan", "-no-color", "-input=false", "-out=plan.bin")...)...)
	plan.requireSuccess(t, "mgcl run -- tofu plan (public function)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "plan output", plan.output(), token1)
	assertNoSecrets(t, "plan output", plan.output(), ephemeralVal)

	capture2 := filepath.Join(work, "env2.txt")
	applyEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	apply := runMgcl(t, mgcl, work, applyEnv, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve", "plan.bin")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (public function)")

	_, token2 := readCapture(t, capture2)
	assertNoToken(t, "apply output", apply.output(), token1)
	assertNoToken(t, "apply output", apply.output(), token2)

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{publicRegion, publicEnvName, `"region"`, `"env"`} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist public function result %q:\n%s", want, state)
		}
	}

	if strings.Contains(state, ephemeralVal) {
		t.Fatalf("state contains the ephemeral KV that the public function must not return:\n%s", state)
	}

	assertNoToken(t, "state file", state, token1)
	assertNoToken(t, "state file", state, token2)

	// The function cannot read an ephemeral KV.
	workEphemeral := t.TempDir()
	cliConfigEph := writeFile(t, workEphemeral, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, workEphemeral, "main.tf", providerBlock+`
resource "terraform_data" "token" {
  input = provider::mgcl::secret_value("`+secretName+`/token")
}
`)

	capture3 := filepath.Join(workEphemeral, "env3.txt")
	ephemeralEnv := tofuEnv(t, cliConfigEph, map[string]string{"MGCL_E2E_CAPTURE": capture3})

	ephemeralPlan := runMgcl(t, mgcl, workEphemeral, ephemeralEnv, mgclRunArgs(db, key,
		captureCommand(capture3, tofu, "plan", "-no-color", "-input=false")...)...)
	ephemeralPlan.requireFailure(t, "mgcl run -- tofu plan (public function on an ephemeral KV)")

	_, token3 := readCapture(t, capture3)

	ephemeralOutput := ephemeralPlan.output()
	normalizedEphemeral := strings.Join(strings.Fields(ephemeralOutput), " ")
	if !strings.Contains(normalizedEphemeral, "was not found among the public KVs") {
		t.Fatalf("failure does not explain the public-only set:\n%s", ephemeralOutput)
	}

	assertNoSecrets(t, "ephemeral failure output", ephemeralOutput, ephemeralVal, publicRegion)
	assertNoToken(t, "ephemeral failure output", ephemeralOutput, token1)
	assertNoToken(t, "ephemeral failure output", ephemeralOutput, token3)

	// mgcl_secret returns every KV of the mixed Secret, including the
	// ephemeral one, without persisting them.
	workRead := t.TempDir()
	cliConfigRead := writeFile(t, workRead, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, workRead, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "mixed" {
  name = "`+secretName+`"
}

resource "terraform_data" "probe" {
  provisioner "local-exec" {
    command = "printf '%s|%s|%s' '${ephemeral.mgcl_secret.mixed.values["region"]}' '${ephemeral.mgcl_secret.mixed.values["env"]}' '${ephemeral.mgcl_secret.mixed.values["token"]}' > values.txt"
  }
}
`)

	capture4 := filepath.Join(workRead, "env4.txt")
	readEnv := tofuEnv(t, cliConfigRead, map[string]string{"MGCL_E2E_CAPTURE": capture4})

	readApply := runMgcl(t, mgcl, workRead, readEnv, mgclRunArgs(db, key,
		captureCommand(capture4, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	readApply.requireSuccess(t, "mgcl run -- tofu apply (mixed mgcl_secret)")

	_, token4 := readCapture(t, capture4)
	requireFileContent(t, filepath.Join(workRead, "values.txt"), publicRegion+"|"+publicEnvName+"|"+ephemeralVal)

	readState := readFileString(t, filepath.Join(workRead, "terraform.tfstate"))
	assertNoSecrets(t, "mixed state file", readState, ephemeralVal, publicRegion)
	assertNoToken(t, "mixed state file", readState, token4)
	assertNoSecrets(t, "mixed apply output", readApply.output(), ephemeralVal, publicRegion)
}

// TestRealEphemeralSecretValueFunction reads a mixed Secret through the real
// mgcl CLI, provider API and OpenTofu. The all-KV function returns public and
// ephemeral values as ordinary data, so both are persisted in state; only the
// provider token must stay out of output and state.
func TestRealEphemeralSecretValueFunction(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	secretEnv := tofuEnv(t, "", nil)

	const (
		secretName   = "app-config"
		publicRegion = "us-east-1"
		ephemeralVal = "EPHEMERAL-REAL-VALUE"
	)

	secretCLI(t, mgcl, secretEnv,
		"secret", "create", secretName, "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", secretName+"/region", publicRegion, "--public", "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", secretName+"/token", ephemeralVal, "--db", db, "--encryption-key-file", key)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, work, "main.tf", providerBlock+`
resource "terraform_data" "single" {
  input = `+ephemeralValueFunction+`("`+secretName+`/token")
}

resource "terraform_data" "wildcard" {
  input = `+ephemeralValueFunction+`("`+secretName+`/*")
}
`)

	capture1 := filepath.Join(work, "env1.txt")
	planEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	plan := runMgcl(t, mgcl, work, planEnv, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "plan", "-no-color", "-input=false", "-out=plan.bin")...)...)
	plan.requireSuccess(t, "mgcl run -- tofu plan (all-KV function)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "plan output", plan.output(), token1)

	capture2 := filepath.Join(work, "env2.txt")
	applyEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	apply := runMgcl(t, mgcl, work, applyEnv, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve", "plan.bin")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (all-KV function)")

	_, token2 := readCapture(t, capture2)
	assertNoToken(t, "apply output", apply.output(), token1)
	assertNoToken(t, "apply output", apply.output(), token2)

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	for _, want := range []string{publicRegion, ephemeralVal, `"region"`, `"token"`} {
		if !strings.Contains(state, want) {
			t.Fatalf("state does not persist all-KV function result %q:\n%s", want, state)
		}
	}

	assertNoToken(t, "state file", state, token1)
	assertNoToken(t, "state file", state, token2)
}
