package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type realSecretView struct {
	Name string `json:"name"`
	Keys []struct {
		Key         string `json:"key"`
		IsEphemeral bool   `json:"is_ephemeral"`
	} `json:"keys"`
}

func readRealSecretView(t *testing.T, mgcl string, env []string, db, name string) realSecretView {
	t.Helper()

	show := secretCLI(t, mgcl, env, "secret", "show", name, "-o", "json", "--db", db)

	var view realSecretView
	if err := json.Unmarshal([]byte(show), &view); err != nil {
		t.Fatalf("decoding secret show JSON: %v\n%s", err, show)
	}

	return view
}

func realSecretMarks(view realSecretView) map[string]bool {
	marks := make(map[string]bool, len(view.Keys))
	for _, key := range view.Keys {
		marks[key.Key] = key.IsEphemeral
	}

	return marks
}

// TestRealManagedSecretLifecycle exercises the managed mgcl_secret against the
// real provider API: function-fed public values, per-entry marks, a no-op
// second apply, an update that adds and removes keys, and a destroy that keeps
// the Secret and unmanaged KVs.
func TestRealManagedSecretLifecycle(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	const (
		source    = "aws-605592676484-cn/source"
		target    = "aws-605592676484-cn/scarborough"
		accountID = "1234"
		region    = "cn-north-1"
	)

	secretEnv := tofuEnv(t, "", nil)

	secretCLI(t, mgcl, secretEnv, "secret", "create", source, "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv, "secret", "set", source+"/account_id", accountID, "--public", "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv, "secret", "set", source+"/region", region, "--public", "--db", db, "--encryption-key-file", key)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "target" {
  name = "`+target+`"
  contents = {
    account_id = {
      value          = provider::mgcl::secret_value("`+source+`/account_id")
      ephemeral_mark = false
    }
    region = {
      value          = provider::mgcl::secret_value("`+source+`/region")
      ephemeral_mark = false
    }
    secret_key = {
      value = "s3cr3t-value"
    }
  }
}
`)

	capture1 := filepath.Join(work, "env1.txt")
	env1 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	apply := runMgcl(t, mgcl, work, env1, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (managed Secret from function)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "apply output", apply.output(), token1)

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", target+"/account_id", "--db", db, "--encryption-key-file", key); got != accountID {
		t.Fatalf("target account_id = %q, want %q", got, accountID)
	}

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", target+"/region", "--db", db, "--encryption-key-file", key); got != region {
		t.Fatalf("target region = %q, want %q", got, region)
	}

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", target+"/secret_key", "--db", db, "--encryption-key-file", key); got != "s3cr3t-value" {
		t.Fatalf("target secret_key = %q, want s3cr3t-value", got)
	}

	marks := realSecretMarks(readRealSecretView(t, mgcl, secretEnv, db, target))
	if len(marks) != 3 {
		t.Fatalf("target keys = %v, want three", marks)
	}

	if marks["account_id"] || marks["region"] {
		t.Fatalf("marks = %v, want account_id and region public", marks)
	}

	if !marks["secret_key"] {
		t.Fatalf("marks = %v, want secret_key ephemeral by default", marks)
	}

	// The second apply must not change anything.
	capture2 := filepath.Join(work, "env2.txt")
	env2 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	second := runMgcl(t, mgcl, work, env2, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	second.requireSuccess(t, "mgcl run -- tofu apply (no changes)")

	if !strings.Contains(second.output(), "0 added, 0 changed, 0 destroyed") {
		t.Fatalf("second apply was not a no-op:\n%s", second.output())
	}

	// Add a KV that the resource does not own; destroy must keep it.
	secretCLI(t, mgcl, secretEnv, "secret", "set", target+"/untracked", "keep-me", "--db", db, "--encryption-key-file", key)

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "target" {
  name = "`+target+`"
  contents = {
    account_id = {
      value          = "5678"
      ephemeral_mark = true
    }
    secret_key = {
      value          = "rotated-env"
      ephemeral_mark = false
    }
  }
}
`)

	capture3 := filepath.Join(work, "env3.txt")
	env3 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture3})

	update := runMgcl(t, mgcl, work, env3, mgclRunArgs(db, key,
		captureCommand(capture3, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	update.requireSuccess(t, "mgcl run -- tofu apply (update managed Secret)")

	_, token3 := readCapture(t, capture3)
	assertNoToken(t, "update output", update.output(), token3)

	gone := runMgcl(t, mgcl, t.TempDir(), secretEnv, "secret", "get", target+"/region", "--db", db, "--encryption-key-file", key)
	gone.requireFailure(t, "secret get of a removed KV")

	marks = realSecretMarks(readRealSecretView(t, mgcl, secretEnv, db, target))
	if !marks["account_id"] {
		t.Fatalf("marks = %v, want account_id ephemeral after update", marks)
	}

	if marks["secret_key"] {
		t.Fatalf("marks = %v, want secret_key public after update", marks)
	}

	if _, ok := marks["region"]; ok {
		t.Fatalf("marks = %v, removed key region still present", marks)
	}

	// Destroy only owned keys.
	capture4 := filepath.Join(work, "env4.txt")
	env4 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture4})

	destroy := runMgcl(t, mgcl, work, env4, mgclRunArgs(db, key,
		captureCommand(capture4, tofu, "destroy", "-no-color", "-input=false", "-auto-approve")...)...)
	destroy.requireSuccess(t, "mgcl run -- tofu destroy (managed Secret)")

	_, token4 := readCapture(t, capture4)
	assertNoToken(t, "destroy output", destroy.output(), token4)

	final := readRealSecretView(t, mgcl, secretEnv, db, target)
	if len(final.Keys) != 1 || final.Keys[0].Key != "untracked" {
		t.Fatalf("keys after destroy = %+v, want only untracked", final.Keys)
	}

	if got := secretCLI(t, mgcl, secretEnv, "secret", "get", target+"/untracked", "--db", db, "--encryption-key-file", key); got != "keep-me" {
		t.Fatalf("untracked = %q, want keep-me", got)
	}
}

// TestRealManagedSecretSameNameAndIndependentRead declares a managed and an
// ephemeral mgcl_secret with the same name in one project, then reads the
// managed Secret from a separate project with the function and the ephemeral
// resource.
func TestRealManagedSecretSameNameAndIndependentRead(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	const (
		target      = "shared/cred"
		ephemeralKV = "EPHEMERAL-TARGET"
	)

	secretEnv := tofuEnv(t, "", nil)

	// Seed the Secret so the ephemeral read is valid regardless of ordering.
	secretCLI(t, mgcl, secretEnv, "secret", "create", target, "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv, "secret", "set", target+"/seed", "seed-value", "--db", db, "--encryption-key-file", key)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, work, "main.tf", providerBlock+`
resource "mgcl_secret" "shared" {
  name = "`+target+`"
  contents = {
    account_id = { value = "1234", ephemeral_mark = false }
    secret_key = { value = "`+ephemeralKV+`" }
  }
}

ephemeral "mgcl_secret" "shared" {
  name = "`+target+`"
}

check "ephemeral_read" {
  assert {
    condition     = ephemeral.mgcl_secret.shared.values["seed"] == "seed-value"
    error_message = "the ephemeral resource must read the same-named Secret"
  }
}
`)

	capture1 := filepath.Join(work, "env1.txt")
	env1 := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture1})

	validate := runMgcl(t, mgcl, work, env1, "run", "--host", "127.0.0.1:0", "--db", db, "--encryption-key-file", key, "--", tofu, "validate", "-no-color")
	validate.requireSuccess(t, "validate with same-named managed and ephemeral resources")

	plan := runMgcl(t, mgcl, work, env1, "run", "--host", "127.0.0.1:0", "--db", db, "--encryption-key-file", key, "--", tofu, "plan", "-no-color", "-input=false")
	plan.requireSuccess(t, "plan with same-named managed and ephemeral resources")

	apply := runMgcl(t, mgcl, work, env1, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply (same-named managed and ephemeral)")

	_, token1 := readCapture(t, capture1)
	assertNoToken(t, "apply output", apply.output(), token1)

	state := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	if strings.Contains(state, "seed-value") {
		t.Fatal("ephemeral same-name read leaked into state")
	}

	if !strings.Contains(state, ephemeralKV) {
		t.Fatal("managed contents were not persisted in state as designed")
	}

	// Independent project reads the managed Secret.
	readWork := t.TempDir()
	readConfig := writeFile(t, readWork, "dev.tfrc", devOverridesConfig(t, binDir))

	writeFile(t, readWork, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "target" {
  name = "`+target+`"
}

resource "terraform_data" "public" {
  input = provider::mgcl::secret_value("`+target+`/account_id")
}

check "ephemeral_read" {
  assert {
    condition     = ephemeral.mgcl_secret.target.values["secret_key"] == "`+ephemeralKV+`"
    error_message = "the independent project must read the ephemeral KV"
  }
}
`)

	capture2 := filepath.Join(readWork, "env2.txt")
	readEnv := tofuEnv(t, readConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	readApply := runMgcl(t, mgcl, readWork, readEnv, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	readApply.requireSuccess(t, "mgcl run -- tofu apply (independent read project)")

	_, token2 := readCapture(t, capture2)
	assertNoToken(t, "read apply output", readApply.output(), token2)

	readState := readFileString(t, filepath.Join(readWork, "terraform.tfstate"))
	if !strings.Contains(readState, "1234") {
		t.Fatal("independent project did not persist the public function result")
	}

	if strings.Contains(readState, ephemeralKV) {
		t.Fatal("independent project leaked the ephemeral KV into state")
	}

	assertNoSecrets(t, "read apply output", readApply.output(), ephemeralKV)
}
