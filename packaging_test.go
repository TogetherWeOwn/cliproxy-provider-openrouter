package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRegistryVersionMatches(t *testing.T) {
	var reg struct {
		Plugins []struct {
			ID, Version, License, Repository string
			Install                          struct{ Type string }
		}
	}
	raw, err := os.ReadFile("registry.json")
	if err != nil || json.Unmarshal(raw, &reg) != nil || len(reg.Plugins) != 1 {
		t.Fatalf("registry %v", err)
	}
	p := reg.Plugins[0]
	if version != "0.3.0" {
		t.Fatalf("version constant %s", version)
	}
	if p.ID != provider || p.Version != version || p.License != "MIT" || p.Install.Type != "github-release" || p.Repository != "https://github.com/TogetherWeOwn/cliproxy-provider-openrouter" {
		t.Fatalf("%+v", p)
	}
}

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target is linux/amd64")
	}
	out := t.TempDir()
	cmd := exec.Command("bash", "scripts/package-release.sh", out)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, result)
	}
	archive := filepath.Join(out, "openrouter_0.3.0_linux_amd64.zip")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	checksums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), hex.EncodeToString(sum[:])+"  openrouter_0.3.0_linux_amd64.zip") {
		t.Fatalf("incorrect checksums %s", checksums)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	found := false
	for _, f := range reader.File {
		if f.Name == "openrouter.so" && f.UncompressedSize64 > 0 {
			found = true
		}
		if strings.Contains(f.Name, "/") || strings.HasSuffix(f.Name, ".h") {
			t.Fatalf("unexpected archive entry %q", f.Name)
		}
	}
	if !found {
		t.Fatal("archive lacks openrouter.so")
	}
}
