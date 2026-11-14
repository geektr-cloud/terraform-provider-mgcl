package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// minTofuMinor is the first OpenTofu minor release with ephemeral resources.
const minTofuMinor = 11

// providerVersion is the version the e2e binary reports. The filesystem mirror
// layout and the built binary must agree.
const providerVersion = "0.1.0"

var (
	moduleRoot = findModuleRoot()

	buildMu   sync.Mutex
	buildDirs = map[string]string{}

	tofuOnce sync.Once
	tofuPath string
	tofuErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()

	cleanupBuilds()

	os.Exit(code)
}

func findModuleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate e2e harness source file")
	}

	return filepath.Dir(filepath.Dir(file))
}

// requireTofu returns the tofu binary path, skipping the test when OpenTofu is
// missing or too old, or when -short is set.
func requireTofu(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping OpenTofu e2e test in -short mode")
	}

	tofuOnce.Do(func() {
		tofuPath, tofuErr = exec.LookPath("tofu")
	})

	if tofuErr != nil {
		t.Skipf("tofu not found in PATH (%v); install OpenTofu >= 1.%d to run e2e tests", tofuErr, minTofuMinor)
	}

	major, minor, err := tofuVersion(t, tofuPath)
	if err != nil {
		t.Skipf("cannot determine tofu version: %v", err)
	}

	if major < 1 || (major == 1 && minor < minTofuMinor) {
		t.Skipf("tofu %d.%d is too old; ephemeral resources need OpenTofu >= 1.%d", major, minor, minTofuMinor)
	}

	return tofuPath
}

var tofuVersionPattern = regexp.MustCompile(`v(\d+)\.(\d+)`)

func tofuVersion(t *testing.T, path string) (int, int, error) {
	t.Helper()

	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("tofu version: %w: %s", err, out)
	}

	match := tofuVersionPattern.FindStringSubmatch(string(out))
	if match == nil {
		return 0, 0, fmt.Errorf("unexpected tofu version output: %q", out)
	}

	major, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, err
	}

	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, err
	}

	return major, minor, nil
}

// buildProvider builds terraform-provider-mgcl with the requested reported
// version and returns the directory containing the binary. Builds are cached
// for the duration of the test binary.
func buildProvider(t *testing.T, version string) string {
	t.Helper()

	buildMu.Lock()
	defer buildMu.Unlock()

	if dir, ok := buildDirs[version]; ok {
		return dir
	}

	dir, err := os.MkdirTemp("", "mgcl-provider-bin-*")
	if err != nil {
		t.Fatalf("creating provider build dir: %v", err)
	}

	binary := filepath.Join(dir, "terraform-provider-mgcl")

	cmd := exec.Command("go", "build", "-o", binary, "-ldflags", "-X main.version="+version, ".")
	cmd.Dir = moduleRoot
	cmd.Env = os.Environ()

	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("building provider binary: %v\n%s", err, out)
	}

	buildDirs[version] = dir

	return dir
}

// writeFile writes content into a test work directory.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", name, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	return path
}

// devOverridesConfig renders a CLI config that points the provider at a local
// binary directory and skips installation.
func devOverridesConfig(t *testing.T, binDir string) string {
	t.Helper()

	return fmt.Sprintf(`provider_installation {
  dev_overrides {
    "geektr-cloud/mgcl" = %q
  }
  direct {}
}
`, binDir)
}

// filesystemMirrorConfig renders a CLI config using an unpacked filesystem
// mirror for the mgcl provider only.
func filesystemMirrorConfig(t *testing.T, mirrorDir string) string {
	t.Helper()

	return fmt.Sprintf(`provider_installation {
  filesystem_mirror {
    path    = %q
    include = ["registry.opentofu.org/geektr-cloud/mgcl"]
  }
  direct {
    exclude = ["registry.opentofu.org/geektr-cloud/mgcl"]
  }
}
`, mirrorDir)
}

// installFilesystemMirror lays out the provider in the unpacked OpenTofu
// filesystem mirror layout and returns the mirror root.
func installFilesystemMirror(t *testing.T, binDir, version string) string {
	t.Helper()

	mirror := t.TempDir()
	platform := runtime.GOOS + "_" + runtime.GOARCH

	targetDir := filepath.Join(mirror, "registry.opentofu.org", "geektr-cloud", "mgcl", version, platform)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("creating mirror layout: %v", err)
	}

	source := filepath.Join(binDir, "terraform-provider-mgcl")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("reading provider binary: %v", err)
	}

	target := filepath.Join(targetDir, "terraform-provider-mgcl_v"+version)
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatalf("writing mirror binary: %v", err)
	}

	return mirror
}

// installPackedFilesystemMirror lays the provider out as a packed provider zip
// (the second layout OpenTofu accepts) and returns the mirror root.
func installPackedFilesystemMirror(t *testing.T, binDir, version string) string {
	t.Helper()

	mirror := t.TempDir()
	platform := runtime.GOOS + "_" + runtime.GOARCH

	targetDir := filepath.Join(mirror, "registry.opentofu.org", "geektr-cloud", "mgcl")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("creating packed mirror layout: %v", err)
	}

	source, err := os.Open(filepath.Join(binDir, "terraform-provider-mgcl"))
	if err != nil {
		t.Fatalf("opening provider binary: %v", err)
	}
	defer source.Close()

	archivePath := filepath.Join(targetDir, fmt.Sprintf("terraform-provider-mgcl_%s_%s.zip", version, platform))

	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("creating packed mirror archive: %v", err)
	}

	writer := zip.NewWriter(archive)

	header := &zip.FileHeader{
		Name:   "terraform-provider-mgcl_v" + version,
		Method: zip.Deflate,
	}
	header.SetMode(0o755)

	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatalf("creating packed mirror entry: %v", err)
	}

	if _, err := io.Copy(entry, source); err != nil {
		t.Fatalf("packing provider binary: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("closing packed mirror archive: %v", err)
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("closing packed mirror file: %v", err)
	}

	return mirror
}

// tofuEnv builds a hermetic environment for tofu runs. HOME is isolated so a
// developer's ~/.tofurc cannot leak in; TF_CLI_CONFIG_FILE must be explicit.
func tofuEnv(t *testing.T, cliConfig string, extra map[string]string) []string {
	t.Helper()

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"CHECKPOINT_DISABLE=1",
	}

	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}

	if cliConfig != "" {
		env = append(env, "TF_CLI_CONFIG_FILE="+cliConfig)
	}

	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}

	// Deterministic environment order helps when debugging failures.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}

	return env
}

type tofuResult struct {
	stdout string
	stderr string
	err    error
}

func (r tofuResult) output() string {
	return r.stdout + "\n" + r.stderr
}

func (r tofuResult) requireSuccess(t *testing.T, args ...string) {
	t.Helper()

	if r.err != nil {
		t.Fatalf("tofu %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.err, r.stdout, r.stderr)
	}
}

func (r tofuResult) requireFailure(t *testing.T, args ...string) {
	t.Helper()

	if r.err == nil {
		t.Fatalf("tofu %s unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.stdout, r.stderr)
	}
}

// runTofu executes tofu in dir with the given environment and arguments.
func runTofu(t *testing.T, tofu, dir string, env []string, args ...string) tofuResult {
	t.Helper()

	return runCommand(t, "tofu", tofu, dir, env, args...)
}

// runCommand executes a binary in dir with the given environment and captures
// stdout/stderr. It is shared by the OpenTofu and mgcl e2e helpers.
func runCommand(t *testing.T, label, binary, dir string, env []string, args ...string) tofuResult {
	t.Helper()

	t.Logf("running: %s %s (dir %s)", label, strings.Join(args, " "), dir)

	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return tofuResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// fakeProviderAPI implements the mgcl provider API (POST
// /api/mgcl/v1/secrets:read) for e2e tests. It records requests and serves the
// configured values, failing closed on a wrong bearer token.
type fakeProviderAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	values   map[string]string
	token    string
	requests []apiRequest
}

type apiRequest struct {
	method string
	path   string
	token  string
	name   string
}

func newFakeProviderAPI(t *testing.T, token string) *fakeProviderAPI {
	t.Helper()

	api := &fakeProviderAPI{
		values: map[string]string{"access_key_id": "AKIAE2E", "secret_access_key": "e2e-s3cr3t-value"},
		token:  token,
	}

	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		var payload struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &payload)

		api.mu.Lock()
		api.requests = append(api.requests, apiRequest{
			method: req.Method,
			path:   req.URL.Path,
			token:  strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "),
			name:   payload.Name,
		})
		values := api.values
		expectedToken := api.token
		api.mu.Unlock()

		w.Header().Set("Cache-Control", "no-store")

		if req.Method != http.MethodPost || req.URL.Path != "/api/mgcl/v1/secrets:read" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)

			return
		}

		if strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ") != expectedToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":   payload.Name,
			"values": values,
		})
	}))
	t.Cleanup(api.server.Close)

	return api
}

func (a *fakeProviderAPI) url() string {
	return a.server.URL
}

func (a *fakeProviderAPI) setValues(values map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.values = values
}

func (a *fakeProviderAPI) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.requests)
}

func (a *fakeProviderAPI) requestList() []apiRequest {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]apiRequest, len(a.requests))
	copy(out, a.requests)

	return out
}

// assertNoSecrets fails when any of the secrets appears in text. Used for
// stdout, stderr, plan files, state files and logs.
func assertNoSecrets(t *testing.T, label, text string, secrets ...string) {
	t.Helper()

	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaks secret value %q:\n%s", label, secret, text)
		}
	}
}

func assertNoToken(t *testing.T, label, text, token string) {
	t.Helper()

	if strings.Contains(text, token) {
		t.Fatalf("%s leaks the provider token", label)
	}
}
