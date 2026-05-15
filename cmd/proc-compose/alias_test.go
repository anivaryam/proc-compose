package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func buildTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "proc-compose_test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build proc-compose: %v\n%s", err, out)
	}
	return bin
}

func TestAliasEquivalence(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		cmd   string
		args  []string
	}{
		{"up_alias", "u", "up", []string{"--help"}},
		{"monitor_alias", "m", "monitor", []string{"--help"}},
		// "s" alias intentionally removed from stop - conflicts with "up -s"
		{"list_alias", "l", "list", []string{"--help"}},
		{"init_alias", "i", "init", []string{"--help"}},
		{"restart_alias_with_arg", "r", "restart", []string{"--help", "backend"}},
		{"reload_alias", "rl", "reload", []string{"--help"}},
	}

	bin := buildTestBinary(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build canonical command with args
			canonicalArgs := append([]string{bin, tt.cmd}, tt.args...)
			canonical := exec.Command(canonicalArgs[0], canonicalArgs[1:]...)
			canonicalOut, canonicalErr := canonical.CombinedOutput()
			canonicalExit := canonical.ProcessState.ExitCode()

			// Build alias command with same args
			aliasArgs := append([]string{bin, tt.alias}, tt.args...)
			aliasCmd := exec.Command(aliasArgs[0], aliasArgs[1:]...)
			aliasOut, aliasErr := aliasCmd.CombinedOutput()
			aliasExit := aliasCmd.ProcessState.ExitCode()

			// Compare exit codes
			if canonicalExit != aliasExit {
				t.Errorf("Exit code mismatch: canonical=%d, alias=%d",
					canonicalExit, aliasExit)
			}

			// Compare output
			if string(canonicalOut) != string(aliasOut) {
				t.Errorf("Output mismatch:\nCanonical: %s\nAlias: %s", canonicalOut, aliasOut)
			}

			// Error should be similar
			if (canonicalErr == nil) != (aliasErr == nil) {
				t.Errorf("Error state mismatch: canonical_err=%v, alias_err=%v",
					canonicalErr, aliasErr)
			}
		})
	}
}

func TestInvalidAlias(t *testing.T) {
	bin := buildTestBinary(t)

	// Test invalid alias
	cmd := exec.Command(bin, "xyz")
	_, err := cmd.CombinedOutput()

	// Should fail with "unknown command" or similar
	if err == nil {
		t.Error("Expected error for invalid alias 'xyz', got nil")
	}
}

func TestSymlinkCreation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Symlink test not applicable on Windows")
	}

	t.Log("Testing symlink creation...")

	tmpDir, err := os.MkdirTemp("", "pc-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	fakeBin := tmpDir + "/proc-compose"
	if err := os.WriteFile(fakeBin, []byte("#!/bin/bash\necho test"), 0755); err != nil {
		t.Fatal(err)
	}

	symlinkPath := tmpDir + "/pc"
	if err := os.Symlink("proc-compose", symlinkPath); err != nil {
		t.Fatal("Failed to create symlink:", err)
	}

	info, err := os.Lstat(symlinkPath)
	if err != nil {
		t.Fatal("Failed to stat symlink:", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("pc is not a symlink")
	}

	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatal("Failed to read symlink:", err)
	}
	if target != "proc-compose" {
		t.Errorf("Symlink points to %s, expected 'proc-compose'", target)
	}

	execPath := tmpDir + "/pc"
	cmd := exec.Command(execPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Symlink not executable: %v, output: %s", err, output)
	}
	if string(output) != "test\n" {
		t.Errorf("Symlink output mismatch: got %q, expected %q", string(output), "test\n")
	}
}
