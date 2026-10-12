package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "proc-compose-test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func runVerifyCLI(t *testing.T, bin, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

// decodeOne asserts stdout holds exactly one JSON object.
func decodeOne(t *testing.T, stdout string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if decoder.More() {
		t.Fatalf("stdout holds more than one JSON value:\n%s", stdout)
	}
	return result
}

func TestVerifyCLI_ExitCodesAndJSONContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture service uses POSIX sleep; Windows covered by internal/verify")
	}
	bin := buildCLI(t)
	dir := t.TempDir()
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    cmd: echo service-log; exec sleep 30\n")

	stdout, stderr, code := runVerifyCLI(t, bin, dir, "--session", "cli", "verify", "--json", "--", "sh", "-c", "echo check-output")
	result := decodeOne(t, stdout)
	if code != 0 || result["status"] != "passed" || result["kind"] != "verify.response" {
		t.Fatalf("pass: code %d result %v\nstderr: %s", code, result, stderr)
	}
	if strings.Contains(stdout, "service-log") || strings.Contains(stderr, "service-log") {
		t.Fatalf("--json streamed application output without --stream-logs\nstderr: %s", stderr)
	}

	stdout, stderr, code = runVerifyCLI(t, bin, dir, "--session", "cli", "verify", "--json", "--stream-logs", "--", "true")
	decodeOne(t, stdout)
	if code != 0 || strings.Contains(stdout, "service-log") || !strings.Contains(stderr, "service-log") {
		t.Fatalf("--stream-logs: code %d stderr %q", code, stderr)
	}

	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--json", "--", "false")
	if result := decodeOne(t, stdout); code != 1 || result["status"] != "failed" {
		t.Fatalf("failing check: code %d result %v", code, result)
	}

	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--json", "--check-timeout", "0s", "--", "true")
	if result := decodeOne(t, stdout); code != 2 || result["phase"] != "validation" {
		t.Fatalf("bad duration: code %d result %v", code, result)
	}

	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--json", "true")
	if result := decodeOne(t, stdout); code != 2 || result["run_id"] != nil {
		t.Fatalf("missing --: code %d result %v", code, result)
	}

	stdout, stderr, code = runVerifyCLI(t, bin, dir, "verify", "--json", "--bogus", "--", "true")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "unknown flag") {
		t.Fatalf("parser exception: code %d stdout %q stderr %q", code, stdout, stderr)
	}

	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--", "true")
	if code != 0 || !strings.Contains(stdout, "verify PASSED") || !strings.Contains(stdout, "cleanup:  passed") {
		t.Fatalf("human: code %d\n%s", code, stdout)
	}
}

func TestVerifyCLI_InvalidConfigJSON(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    depends_on: [missing]\n")
	stdout, _, code := runVerifyCLI(t, bin, dir, "verify", "--json", "--", "true")
	result := decodeOne(t, stdout)
	if code != 2 || result["receipt_persisted"] != false || result["attempt_id"] != nil {
		t.Fatalf("invalid config: code %d result %v", code, result)
	}
}

// TestVerifyCLI_InvalidSessionIsStructured drives the real entry point: the
// root pre-run hook validates --session, and commands that promise structured
// output must still emit it, with the usage exit status.
func TestVerifyCLI_InvalidSessionIsStructured(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    cmd: sleep 30\n")

	stdout, stderr, code := runVerifyCLI(t, bin, dir, "--session", "invalid space", "verify", "--json", "--", "true")
	result := decodeOne(t, stdout)
	if code != 2 || result["kind"] != "verify.response" || result["exit_code"] != float64(2) || result["attempt_id"] != nil || !strings.HasSuffix(result["config_path"].(string), "proc-compose.yml") {
		t.Fatalf("verify --json: code %d result %v", code, result)
	}
	if errs, _ := result["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["code"] != "usage_error" {
		t.Fatalf("verify --json errors = %v", result["errors"])
	}
	if !strings.Contains(stderr, "invalid --session") {
		t.Fatalf("verify --json stderr = %q", stderr)
	}

	stdout, stderr, code = runVerifyCLI(t, bin, dir, "--session", "invalid space", "verify", "--", "true")
	if code != 2 || !strings.Contains(stdout, "verify FAILED (exit 2)") || !strings.Contains(stderr, "invalid --session") {
		t.Fatalf("verify human: code %d stdout %q stderr %q", code, stdout, stderr)
	}

	stdout, _, code = runVerifyCLI(t, bin, dir, "--session", "invalid space", "compare", "--baseline", dir, "--candidate", dir, "--json", "--", "true")
	comparison := decodeOne(t, stdout)
	if code != 2 || comparison["kind"] != "verify.comparison" || comparison["status"] != "not_comparable" {
		t.Fatalf("compare --json: code %d result %v", code, comparison)
	}

	// Commands without a structured contract keep the plain error.
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "--session", "invalid space", "status")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid --session") {
		t.Fatalf("status: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}
