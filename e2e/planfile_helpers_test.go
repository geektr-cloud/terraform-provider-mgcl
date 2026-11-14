package e2e

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxPlanEntryBytes bounds how much a single plan archive entry may expand to.
// Plan files are produced by the local tofu binary, but the limit keeps a
// corrupt or hostile archive from exhausting test memory.
const maxPlanEntryBytes = 64 << 20

// planFileLeaks reports every secret value or provider token found in the
// decompressed entries of a binary OpenTofu plan file.
//
// Binary plan files are zip archives and are normally deflated, so scanning the
// raw file bytes would miss values. Every non-directory entry is read and
// checked individually.
func planFileLeaks(path string, secrets []string, token string) ([]string, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open plan archive: %w", err)
	}
	defer archive.Close()

	var leaks []string

	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() {
			continue
		}

		data, err := readPlanEntry(entry)
		if err != nil {
			return nil, err
		}

		text := string(data)

		for _, secret := range secrets {
			if secret != "" && strings.Contains(text, secret) {
				leaks = append(leaks, fmt.Sprintf("entry %q contains secret value %q", entry.Name, secret))
			}
		}

		if token != "" && strings.Contains(text, token) {
			leaks = append(leaks, fmt.Sprintf("entry %q contains the provider token", entry.Name))
		}
	}

	return leaks, nil
}

func readPlanEntry(entry *zip.File) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open plan entry %q: %w", entry.Name, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, maxPlanEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read plan entry %q: %w", entry.Name, err)
	}

	if len(data) > maxPlanEntryBytes {
		return nil, fmt.Errorf("plan entry %q exceeds %d bytes", entry.Name, maxPlanEntryBytes)
	}

	return data, nil
}

// assertPlanFileNoLeaks scans every decompressed entry of a binary plan file
// for secret values and the provider token.
func assertPlanFileNoLeaks(t *testing.T, label, path, token string, secrets ...string) {
	t.Helper()

	leaks, err := planFileLeaks(path, secrets, token)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}

	if len(leaks) > 0 {
		t.Fatalf("%s leaks secret material: %s", label, strings.Join(leaks, "; "))
	}
}

// TestPlanFileScannerDetectsCompressedMarker proves the plan scanner is not a
// no-op: the marker exists only inside a deflated entry, raw plan bytes do not
// contain it, and the scanner still reports it.
func TestPlanFileScannerDetectsCompressedMarker(t *testing.T) {
	const marker = "plan-scanner-marker-9f3c"

	dir := t.TempDir()
	path := filepath.Join(dir, "plan.bin")

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating plan file: %v", err)
	}

	writer := zip.NewWriter(file)

	entry, err := writer.CreateHeader(&zip.FileHeader{
		Name:   "tfplan",
		Method: zip.Deflate,
	})
	if err != nil {
		t.Fatalf("creating plan entry: %v", err)
	}

	// Repetitive padding guarantees the marker is not stored verbatim.
	if _, err := entry.Write([]byte(strings.Repeat("padding ", 2048) + marker)); err != nil {
		t.Fatalf("writing plan entry: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("closing plan archive: %v", err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("closing plan file: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading plan file: %v", err)
	}

	if strings.Contains(string(raw), marker) {
		t.Fatal("test setup is ineffective: the compressed plan bytes contain the marker verbatim")
	}

	leaks, err := planFileLeaks(path, []string{marker}, "")
	if err != nil {
		t.Fatalf("scanning plan file: %v", err)
	}

	if len(leaks) != 1 {
		t.Fatalf("scanner missed the compressed marker: leaks = %v", leaks)
	}

	leaks, err = planFileLeaks(path, nil, marker)
	if err != nil {
		t.Fatalf("scanning plan file for the token: %v", err)
	}

	if len(leaks) != 1 {
		t.Fatalf("scanner missed the compressed token marker: leaks = %v", leaks)
	}

	leaks, err = planFileLeaks(path, []string{"a-marker-that-is-absent"}, "")
	if err != nil {
		t.Fatalf("scanning plan file: %v", err)
	}

	if len(leaks) != 0 {
		t.Fatalf("scanner reported an absent marker: leaks = %v", leaks)
	}
}
