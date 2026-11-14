package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

// TestRealValidateWithoutSession proves the real provider binary validates
// without any mgcl provider session and therefore without reading a secret.
func TestRealValidateWithoutSession(t *testing.T) {
	tofu := requireTofu(t)
	binDir := buildProvider(t, "dev")

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+secretEphemeral("bootstrap"))

	env := tofuEnv(t, cliConfig, nil)

	validate := runTofu(t, tofu, work, env, "validate", "-no-color")
	validate.requireSuccess(t, "validate")

	// Without a session the provider must fail at Open with an actionable
	// diagnostic instead of silently reading anything.
	plan := runTofu(t, tofu, work, env, "plan", "-no-color", "-input=false")
	plan.requireFailure(t, "plan")

	if output := plan.output(); !strings.Contains(output, providerapi.EnvEndpoint) {
		t.Fatalf("plan without a session does not mention %s:\n%s", providerapi.EnvEndpoint, output)
	}
}

// TestRealPlanApplyAndRefresh drives the full loop through `mgcl run` and the
// real provider API: plan and apply open the ephemeral resource, a later run
// observes updated values, and no value or token reaches plan, state, output or
// TF_LOG.
func TestRealPlanApplyAndRefresh(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	secretEnv := tofuEnv(t, "", nil)
	prepareSecret(t, mgcl, secretEnv, db, key, "bootstrap", map[string]string{
		"access_key_id":     "AKIA-REAL-1",
		"secret_access_key": "real-s3cr3t-1",
	})

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", applyProbeHCL("bootstrap", "1"))

	secrets := []string{"AKIA-REAL-1", "real-s3cr3t-1"}

	// Plan: the ephemeral resource is opened during plan.
	capture1 := filepath.Join(work, "env1.txt")
	log1 := filepath.Join(work, "plan.log")

	planEnv := tofuEnv(t, cliConfig, map[string]string{
		"MGCL_E2E_CAPTURE": capture1,
		"TF_LOG":           "TRACE",
		"TF_LOG_PATH":      log1,
	})

	plan := runMgcl(t, mgcl, work, planEnv, mgclRunArgs(db, key,
		captureCommand(capture1, tofu, "plan", "-no-color", "-input=false", "-out=plan.bin")...)...)
	plan.requireSuccess(t, "mgcl run -- tofu plan")

	endpoint1, token1 := readCapture(t, capture1)
	if !strings.HasPrefix(endpoint1, "http://127.0.0.1:") {
		t.Fatalf("provider endpoint = %q, want loopback", endpoint1)
	}

	assertNoSecrets(t, "plan output", plan.output(), secrets...)
	assertNoToken(t, "plan output", plan.output(), token1)

	assertPlanFileNoLeaks(t, "plan file", filepath.Join(work, "plan.bin"), token1, secrets...)

	logText := readFileString(t, log1)
	assertNoSecrets(t, "TF_LOG output", logText, secrets...)
	assertNoToken(t, "TF_LOG output", logText, token1)

	// Apply in a separate `mgcl run`, so it gets a fresh provider session.
	capture2 := filepath.Join(work, "env2.txt")
	applyEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture2})

	apply := runMgcl(t, mgcl, work, applyEnv, mgclRunArgs(db, key,
		captureCommand(capture2, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	apply.requireSuccess(t, "mgcl run -- tofu apply")

	_, token2 := readCapture(t, capture2)
	if token2 == token1 {
		t.Fatal("two mgcl runs issued the same provider token, want one per child")
	}

	assertNoSecrets(t, "apply output", apply.output(), secrets...)
	assertNoToken(t, "apply output", apply.output(), token1)
	assertNoToken(t, "apply output", apply.output(), token2)

	requireFileContent(t, filepath.Join(work, "access_key_id.txt"), "AKIA-REAL-1")

	assertNoSecrets(t, "state file", readFileString(t, filepath.Join(work, "terraform.tfstate")), secrets...)
	assertNoToken(t, "state file", readFileString(t, filepath.Join(work, "terraform.tfstate")), token1)
	assertNoToken(t, "state file", readFileString(t, filepath.Join(work, "terraform.tfstate")), token2)

	// Update the secret through the real CLI; the next run must read the new
	// values rather than anything cached from earlier runs.
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", "bootstrap/access_key_id", "AKIA-REAL-2", "--db", db, "--encryption-key-file", key)
	secretCLI(t, mgcl, secretEnv,
		"secret", "set", "bootstrap/secret_access_key", "real-s3cr3t-2", "--db", db, "--encryption-key-file", key)

	writeFile(t, work, "main.tf", applyProbeHCL("bootstrap", "2"))

	capture3 := filepath.Join(work, "env3.txt")
	refreshEnv := tofuEnv(t, cliConfig, map[string]string{"MGCL_E2E_CAPTURE": capture3})

	refresh := runMgcl(t, mgcl, work, refreshEnv, mgclRunArgs(db, key,
		captureCommand(capture3, tofu, "apply", "-no-color", "-input=false", "-auto-approve")...)...)
	refresh.requireSuccess(t, "mgcl run -- tofu apply (refresh)")

	_, token3 := readCapture(t, capture3)

	requireFileContent(t, filepath.Join(work, "access_key_id.txt"), "AKIA-REAL-2")

	allSecrets := []string{"AKIA-REAL-1", "real-s3cr3t-1", "AKIA-REAL-2", "real-s3cr3t-2"}
	stateText := readFileString(t, filepath.Join(work, "terraform.tfstate"))
	assertNoSecrets(t, "state file", stateText, allSecrets...)
	for _, token := range []string{token1, token2, token3} {
		assertNoToken(t, "state file", stateText, token)
	}

	// The refreshed value must also stay out of the refreshed plan output.
	assertNoSecrets(t, "refresh output", refresh.output(), allSecrets...)
	assertNoToken(t, "refresh output", refresh.output(), token3)

	// Plan itself reads through the real API: an unknown secret must fail at
	// plan time instead of being silently skipped.
	writeFile(t, work, "main.tf", providerBlock+secretEphemeral("bootstrap-missing"))

	missingPlan := runMgcl(t, mgcl, work, refreshEnv, mgclRunArgs(db, key,
		captureCommand(filepath.Join(work, "env4.txt"), tofu, "plan", "-no-color", "-input=false")...)...)
	missingPlan.requireFailure(t, "mgcl run -- tofu plan (missing secret)")

	if output := missingPlan.output(); !strings.Contains(output, "Failed to read mgcl secret") {
		t.Fatalf("plan for a missing secret does not show the provider API failure:\n%s", output)
	}

	assertNoSecrets(t, "missing-secret plan output", missingPlan.output(), allSecrets...)
}

// TestRealEmptySecret reads a secret with no KV pairs through the real stack.
func TestRealEmptySecret(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	secretEnv := tofuEnv(t, "", nil)
	prepareSecret(t, mgcl, secretEnv, db, key, "empty", nil)

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "empty" {
  name = "empty"
}

check "empty" {
  assert {
    condition     = length(ephemeral.mgcl_secret.empty.values) == 0
    error_message = "empty secret must read as an empty map"
  }
}
`)

	env := tofuEnv(t, cliConfig, nil)

	plan := runMgcl(t, mgcl, work, env, mgclRunArgs(db, key,
		captureCommand(filepath.Join(work, "env.txt"), tofu, "plan", "-no-color", "-input=false")...)...)
	plan.requireSuccess(t, "mgcl run -- tofu plan (empty secret)")
}

// TestRealPersistentUseRejected proves OpenTofu core rejects an ephemeral value
// used in a persisted argument, on the real provider path.
func TestRealPersistentUseRejected(t *testing.T) {
	tofu := requireTofu(t)
	mgcl := buildMgcl(t)
	binDir := buildProvider(t, "dev")

	home := t.TempDir()
	db := filepath.Join(home, "mgcl.db")
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))

	secretEnv := tofuEnv(t, "", nil)
	prepareSecret(t, mgcl, secretEnv, db, key, "bootstrap", map[string]string{
		"access_key_id": "AKIA-MISUSE",
	})

	work := t.TempDir()
	cliConfig := writeFile(t, work, "dev.tfrc", devOverridesConfig(t, binDir))
	writeFile(t, work, "main.tf", providerBlock+`
ephemeral "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}

resource "terraform_data" "persisted" {
  input = ephemeral.mgcl_secret.bootstrap.values["access_key_id"]
}
`)

	env := tofuEnv(t, cliConfig, nil)

	plan := runMgcl(t, mgcl, work, env, mgclRunArgs(db, key,
		captureCommand(filepath.Join(work, "env.txt"), tofu, "plan", "-no-color", "-input=false")...)...)
	plan.requireFailure(t, "mgcl run -- tofu plan (persistent use)")

	output := plan.output()
	if !strings.Contains(strings.ToLower(output), "ephemeral") {
		t.Fatalf("failure output does not mention ephemeral values:\n%s", output)
	}

	assertNoSecrets(t, "misuse output", output, "AKIA-MISUSE")

	if _, err := os.Stat(filepath.Join(work, "terraform.tfstate")); err == nil {
		t.Fatal("misuse plan wrote a state file")
	}
}

// backgroundRun is a long-running mgcl run whose provider session stays alive
// while the test probes it.
type backgroundRun struct {
	cmd      *exec.Cmd
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	waitOnce sync.Once
	waitErr  error
}

func startBackgroundRun(t *testing.T, binary, dir string, env []string, args ...string) *backgroundRun {
	t.Helper()

	run := &backgroundRun{cmd: exec.Command(binary, args...)}
	run.cmd.Dir = dir
	run.cmd.Env = env
	run.cmd.Stdout = &run.stdout
	run.cmd.Stderr = &run.stderr

	if err := run.cmd.Start(); err != nil {
		t.Fatalf("starting background mgcl run: %v", err)
	}

	return run
}

func (b *backgroundRun) stop(t *testing.T) {
	t.Helper()

	if b.cmd.Process == nil {
		return
	}

	_ = b.cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- b.wait() }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = b.cmd.Process.Kill()
		<-done
		t.Log("background mgcl run did not stop after SIGINT; killed it")
	}
}

func (b *backgroundRun) wait() error {
	b.waitOnce.Do(func() { b.waitErr = b.cmd.Wait() })

	return b.waitErr
}

func (b *backgroundRun) output() string {
	return b.stdout.String() + "\n" + b.stderr.String()
}

// TestRealProviderTokenLifecycle checks the real session token: it works while
// its run is alive, a tampered token and another run's token get 401, and the
// endpoint is unusable after the run exits. Tokens never appear in run output.
func TestRealProviderTokenLifecycle(t *testing.T) {
	requireTofu(t)
	mgcl := buildMgcl(t)

	home := t.TempDir()
	key := writeEd25519Key(t, filepath.Join(home, "id_ed25519"))
	db1 := filepath.Join(home, "one.db")
	db2 := filepath.Join(home, "two.db")

	secretEnv := tofuEnv(t, "", nil)
	prepareSecret(t, mgcl, secretEnv, db1, key, "bootstrap", map[string]string{"access_key_id": "AKIA-TOKEN-1"})
	prepareSecret(t, mgcl, secretEnv, db2, key, "bootstrap", map[string]string{"access_key_id": "AKIA-TOKEN-2"})

	work := t.TempDir()

	startRun := func(db, capture string) *backgroundRun {
		script := `printf '%s\n%s\n' "$MGCL_PROVIDER_ENDPOINT" "$MGCL_PROVIDER_TOKEN" > "$MGCL_E2E_CAPTURE"; exec sleep 300`
		env := tofuEnv(t, "", map[string]string{"MGCL_E2E_CAPTURE": capture})
		args := mgclRunArgs(db, key, "sh", "-c", script)

		return startBackgroundRun(t, mgcl, work, env, args...)
	}

	capture1 := filepath.Join(work, "env1.txt")
	run1 := startRun(db1, capture1)
	t.Cleanup(func() { run1.stop(t) })

	endpoint1, token1 := waitForCapture(t, capture1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &providerapi.Client{Endpoint: endpoint1, Token: token1}
	secret, err := client.ReadSecret(ctx, "bootstrap")
	if err != nil {
		t.Fatalf("reading through the live provider session: %v", err)
	}

	if secret.Values["access_key_id"] != "AKIA-TOKEN-1" {
		t.Fatalf("live secret value = %q, want AKIA-TOKEN-1", secret.Values["access_key_id"])
	}

	status, err := readSecretStatus(ctx, endpoint1, token1+"x", "bootstrap")
	if err != nil {
		t.Fatalf("tampered token request failed at transport level: %v", err)
	}

	if status != 401 {
		t.Fatalf("tampered token status = %d, want 401", status)
	}

	// A second run is a separate session: run one's token must not work there.
	capture2 := filepath.Join(work, "env2.txt")
	run2 := startRun(db2, capture2)
	t.Cleanup(func() { run2.stop(t) })

	endpoint2, token2 := waitForCapture(t, capture2)
	if token1 == token2 {
		t.Fatal("two provider sessions issued the same token")
	}

	status, err = readSecretStatus(ctx, endpoint2, token1, "bootstrap")
	if err != nil {
		t.Fatalf("cross-session request failed at transport level: %v", err)
	}

	if status != 401 {
		t.Fatalf("cross-session token status = %d, want 401", status)
	}

	run2.stop(t)
	run1.stop(t)

	// After the run exits the endpoint is gone or the session is revoked; either
	// way the captured endpoint and token must not read anything.
	status, err = readSecretStatus(context.Background(), endpoint1, token1, "bootstrap")

	postExit := "status " + fmt.Sprint(status)
	if err != nil {
		postExit = err.Error()
	}

	t.Logf("post-exit request for the captured session failed as expected: %s", postExit)

	if err == nil && status == 200 {
		t.Fatal("captured endpoint and token still read the secret after the run exited")
	}

	assertNoToken(t, "run 1 output", run1.output(), token1)
	assertNoToken(t, "run 1 output", run1.output(), token2)
	assertNoToken(t, "run 2 output", run2.output(), token1)
	assertNoToken(t, "run 2 output", run2.output(), token2)
}

// TestRealNonLoopbackGateFailsBeforeSideEffects runs the exact exposure-gate
// command from the design doc in a non-TTY. It must fail before loading the key,
// creating the database or listening.
func TestRealNonLoopbackGateFailsBeforeSideEffects(t *testing.T) {
	requireTofu(t)
	mgcl := buildMgcl(t)

	work := t.TempDir()
	missingKey := filepath.Join(work, "does-not-exist", "id_ed25519")
	dbPath := filepath.Join(work, "missing-dir", "mgcl.db")

	env := tofuEnv(t, "", nil)

	cases := []struct {
		name     string
		host     string
		wantGate bool
	}{
		{name: "host without port", host: "127.0.0.1"},
		{name: "host with ephemeral port", host: "127.0.0.1:0", wantGate: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runMgcl(t, mgcl, work, env,
				"run",
				"--host", tc.host,
				"--server", "http://192.0.2.10:1217",
				"--db", dbPath,
				"--encryption-key-file", missingKey,
				"--", "true",
			)
			res.requireFailure(t, "mgcl run non-loopback without confirmation")

			for _, path := range []string{filepath.Dir(dbPath), filepath.Dir(missingKey)} {
				if _, err := os.Stat(path); err == nil {
					t.Fatalf("gate failure created side-effect directory %s", path)
				}
			}

			if tc.wantGate {
				lowered := strings.ToLower(res.output())
				if !strings.Contains(lowered, "non-loopback") && !strings.Contains(lowered, "refusing") {
					t.Fatalf("failure output does not contain the exposure refusal:\n%s", res.output())
				}
			}
		})
	}
}
