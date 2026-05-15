package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCommandAppearsInHelp(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bootstrap") {
		t.Fatalf("help missing bootstrap:\n%s", out)
	}
}

func TestBootstrapJSON(t *testing.T) {
	tmp := t.TempDir()
	writeCLIFile(t, filepath.Join(tmp, "package.json"), `{"scripts":{"dev":"vite"},"dependencies":{"vite":"latest"}}`)

	bin := filepath.Join(tmp, "proc-compose-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "bootstrap", "--json")
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap --json failed: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, `"mode": "dry_run"`) || !strings.Contains(text, `"suggested_yaml"`) {
		t.Fatalf("unexpected JSON:\n%s", text)
	}
}

func writeCLIFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
