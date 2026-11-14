package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"golang.org/x/crypto/ssh"
)

var (
	mgclBuildOnce sync.Once
	mgclBinary    string
	mgclBuildErr  error
)

// mainRepoRoot locates the openmgcl source tree through the module graph, so
// the cross-repo tests build the same revision that go.mod resolves.
func mainRepoRoot(t *testing.T) string {
	t.Helper()

	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/geektr-cloud/openmgcl")
	cmd.Dir = moduleRoot

	out, err := cmd.Output()
	if err != nil {
		t.Skipf("cannot resolve github.com/geektr-cloud/openmgcl source: %v", err)
	}

	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Skip("github.com/geektr-cloud/openmgcl resolves to an empty directory")
	}

	if _, err := os.Stat(filepath.Join(dir, "cmd", "mgcl", "main.go")); err != nil {
		t.Skipf("openmgcl at %s has no cmd/mgcl entry point: %v", dir, err)
	}

	return dir
}

// buildMgcl builds the real mgcl CLI from the openmgcl source tree once per
// test binary.
func buildMgcl(t *testing.T) string {
	t.Helper()

	root := mainRepoRoot(t)

	mgclBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mgcl-cli-*")
		if err != nil {
			mgclBuildErr = err
			return
		}

		binary := filepath.Join(dir, "mgcl")

		cmd := exec.Command("go", "build", "-o", binary, "./cmd/mgcl")
		cmd.Dir = root
		cmd.Env = os.Environ()

		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			mgclBuildErr = fmt.Errorf("building mgcl from %s: %w\n%s", root, err, out)
			return
		}

		mgclBinary = binary
	})

	if mgclBuildErr != nil {
		t.Fatalf("%v", mgclBuildErr)
	}

	return mgclBinary
}

// cleanupBuilds removes cached provider and mgcl builds when the test binary
// exits.
func cleanupBuilds() {
	buildMu.Lock()
	for _, dir := range buildDirs {
		_ = os.RemoveAll(dir)
	}
	buildMu.Unlock()

	if mgclBinary != "" {
		_ = os.RemoveAll(filepath.Dir(mgclBinary))
	}
}

// writeEd25519Key writes a fresh unencrypted OpenSSH Ed25519 private key with
// mode 0600. Tests never touch the developer's ~/.ssh.
func writeEd25519Key(t *testing.T, path string) string {
	t.Helper()

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating Ed25519 key: %v", err)
	}

	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatalf("marshalling OpenSSH private key: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating key directory: %v", err)
	}

	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing private key: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("private key mode = %o, want 0600", mode)
	}

	return path
}

// runMgcl executes the mgcl binary and captures stdout/stderr.
func runMgcl(t *testing.T, binary, dir string, env []string, args ...string) tofuResult {
	t.Helper()

	return runCommand(t, "mgcl", binary, dir, env, args...)
}

// secretCLI prepares secrets through the real offline CLI.
func secretCLI(t *testing.T, binary string, env []string, args ...string) string {
	t.Helper()

	res := runMgcl(t, binary, t.TempDir(), env, args...)
	res.requireSuccess(t, append([]string{"secret"}, args...)...)

	return res.stdout
}

// prepareSecret creates a secret and sets its values, then verifies the stored
// metadata and one decrypted value with the CLI.
func prepareSecret(t *testing.T, binary string, env []string, db, key, name string, values map[string]string) {
	t.Helper()

	secretCLI(t, binary, env, "secret", "create", name, "--db", db, "--encryption-key-file", key)

	for kvKey, value := range values {
		secretCLI(t, binary, env, "secret", "set", name+"/"+kvKey, value, "--db", db, "--encryption-key-file", key)
	}

	list := secretCLI(t, binary, env, "secret", "list", "--db", db)
	if !strings.Contains(list, name) {
		t.Fatalf("secret list does not mention %q:\n%s", name, list)
	}

	show := secretCLI(t, binary, env, "secret", "show", name, "-o", "json", "--db", db)
	if !strings.Contains(show, `"name": "`+name+`"`) && !strings.Contains(show, `"name":"`+name+`"`) {
		t.Fatalf("secret show does not describe %q:\n%s", name, show)
	}

	for kvKey, want := range values {
		if got := secretCLI(t, binary, env, "secret", "get", name+"/"+kvKey, "--db", db, "--encryption-key-file", key); got != want {
			t.Fatalf("secret get %s/%s = %q, want %q", name, kvKey, got, want)
		}
	}
}

// mgclRunArgs builds `mgcl run` arguments with an ephemeral loopback port.
func mgclRunArgs(db, key string, command ...string) []string {
	args := []string{
		"run",
		"--host", "127.0.0.1:0",
		"--db", db,
		"--encryption-key-file", key,
		"--",
	}

	return append(args, command...)
}

// captureCommand wraps a command so the provider session environment is written
// to a temporary file before the command runs. The file lets tests assert that
// logs never contain the token and that the token is rejected afterwards.
func captureCommand(capturePath, tofu string, args ...string) []string {
	script := `printf '%s\n%s\n' "$MGCL_PROVIDER_ENDPOINT" "$MGCL_PROVIDER_TOKEN" > "$MGCL_E2E_CAPTURE"; exec "$@"`

	return append([]string{"sh", "-c", script, "mgcl-e2e", tofu}, args...)
}

// readCapture parses the captured provider endpoint and token.
func readCapture(t *testing.T, path string) (string, string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading captured provider environment: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		t.Fatalf("captured provider environment is malformed: %q", string(data))
	}

	return lines[0], lines[1]
}

// waitForCapture polls for the capture file written by a background child.
func waitForCapture(t *testing.T, path string) (string, string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(lines) == 2 && lines[0] != "" && lines[1] != "" {
				return lines[0], lines[1]
			}
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("provider environment was not captured at %s", path)

	return "", ""
}

// readSecretStatus calls the real provider API and returns the HTTP status.
// Transport errors are returned separately so callers can distinguish a closed
// listener from a rejected token.
func readSecretStatus(ctx context.Context, endpoint, token, name string) (int, error) {
	body := strings.NewReader(`{"name":"` + name + `"}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+providerapi.SecretsReadPath, body)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// stopProcess interrupts a background mgcl run and waits for it to exit.
func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(os.Interrupt)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		t.Log("mgcl run did not stop after SIGINT; killed it")
	}
}
