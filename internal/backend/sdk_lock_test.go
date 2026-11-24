package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVendoredGoSDKMatchesLockChecksums(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "sdk.lock.json"))
	if err != nil {
		t.Fatalf("read sdk.lock.json: %v", err)
	}
	var lock struct {
		SchemaVersion int `json:"schemaVersion"`
		Source        struct {
			Repository string `json:"repository"`
			Revision   string `json:"revision"`
		} `json:"source"`
		Files []struct {
			Upstream string `json:"upstream"`
			Path     string `json:"path"`
			SHA256   string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("parse sdk.lock.json: %v", err)
	}
	if lock.SchemaVersion != 1 || len(lock.Source.Revision) != 40 || len(lock.Files) == 0 {
		t.Fatalf("invalid sdk.lock.json metadata: %+v", lock)
	}
	for _, f := range lock.Files {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("missing vendored file %s: %v", f.Path, err)
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != f.SHA256 {
			t.Errorf("%s sha256 = %s, want %s (upstream %s)", f.Path, got, f.SHA256, f.Upstream)
		}
	}
}
