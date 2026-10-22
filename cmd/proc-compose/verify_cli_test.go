package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
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
	return runVerifyCLIIn(t, bin, dir, t.TempDir(), args...)
}

// runVerifyCLIIn is runVerifyCLI with runtimeDir as the runtime directory, so
// the command sees sessions a test recorded there.
func runVerifyCLIIn(t *testing.T, bin, dir, runtimeDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "XDG_RUNTIME_DIR="+runtimeDir)
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

	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--json", "--require-environment", "--", "true")
	if result := decodeOne(t, stdout); code != 1 || result["environment"].(map[string]any)["status"] != "unknown" {
		t.Fatalf("--require-environment without a report: code %d result %v", code, result)
	}

	report := `{"observations":[{"name":"api.build","status":"mismatched","expected":"b2","observed":"b1"}]}`
	stdout, _, code = runVerifyCLI(t, bin, dir, "verify", "--", "sh", "-c", `printf '%s' "$1" > "$PROC_COMPOSE_ENVIRONMENT_REPORT"`, "sh", report)
	if code != 1 || !strings.Contains(stdout, "environment: ineligible") || !strings.Contains(stdout, `api.build mismatched: expected "b2", observed "b1"`) {
		t.Fatalf("human environment: code %d\n%s", code, stdout)
	}

	base, cand := t.TempDir(), t.TempDir()
	for _, root := range []string{base, cand} {
		writeCLIFile(t, filepath.Join(root, "proc-compose.yml"), "processes:\n  api:\n    cmd: exec sleep 30\n")
	}
	stdout, _, code = runVerifyCLI(t, bin, dir, "compare", "--json", "--require-environment", "--baseline", base, "--candidate", cand, "--", "true")
	if result := decodeOne(t, stdout); code != 1 || result["status"] != "not_comparable" || !strings.Contains(stdout, `"code":"environment_unverified"`) {
		t.Fatalf("compare --require-environment without reports: code %d result %v", code, result)
	}
	// A repeated member must not let a later "matched" replace a mismatch.
	repeated := `{"observations":[{"name":"api.build","status":"mismatched","status":"matched"}]}`
	stdout, _, code = runVerifyCLI(t, bin, dir, "compare", "--json", "--baseline", base, "--candidate", cand, "--", "sh", "-c", `printf '%s' "$1" > "$PROC_COMPOSE_ENVIRONMENT_REPORT"`, "sh", repeated)
	if result := decodeOne(t, stdout); code != 1 || result["status"] != "not_comparable" || !strings.Contains(stdout, `"code":"environment_unverified"`) {
		t.Fatalf("compare with a repeated status: code %d result %v", code, result)
	}
}

// TestVerifyCLI_RefusesCrossSessionAnswersByDefault drives the real verify
// command: by default, a readiness probe answered on a port another live
// session leases fails startup and names that session; --allow-cross-session
// counts the answer again, the owner keeps the label, and human output names
// the session. macOS attributes nothing, so it passes there either way.
func TestVerifyCLI_RefusesCrossSessionAnswersByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture service uses POSIX sleep; Windows covered by internal/verify")
	}
	bin := buildCLI(t)
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	// A listener that never accepts: each probe connection waits in its
	// queue, so it is attributed to this test process.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	supervisor := exec.Command("sleep", "60")
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = supervisor.Process.Kill()
		_ = supervisor.Wait()
	})
	devConfig := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(devConfig, "dev")
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(addr[strings.LastIndexByte(addr, ':')+1:])
	dev := &ipc.Project{Session: "dev", RunID: "run-dev", ConfigPath: devConfig, Ports: map[string]map[string]int{"web": {"http": port}},
		Owner: &ipc.Owner{PID: supervisor.Process.Pid, Command: "up", Worktree: "/work/dev"}}
	if err := session.Save(paths.Metadata(hash), dev); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WritePID(paths.PID(hash), supervisor.Process.Pid, paths.Socket(hash)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    cmd: exec sleep 30\n    ready_when:\n      tcp: "+addr+"\n    ready_timeout: 1\n")
	owner := func(result map[string]any) map[string]any {
		obs := result["startup"].(map[string]any)["readiness_observations"].([]any)
		owner, _ := obs[0].(map[string]any)["owner"].(map[string]any)
		return owner
	}
	attributed := runtime.GOOS == "linux"

	stdout, stderr, code := runVerifyCLIIn(t, bin, dir, runtimeDir, "verify", "--json", "--", "true")
	result := decodeOne(t, stdout)
	o := owner(result)
	switch {
	case !attributed:
		if code != 0 || o["result"] != "unattributable" {
			t.Fatalf("default on %s: code %d owner %v\nstderr: %s", runtime.GOOS, code, o, stderr)
		}
	case code != 1 || result["status"] != "failed" || result["check"].(map[string]any)["status"] != "not_run" ||
		o["scope"] != "cross_session" || o["session"].(map[string]any)["name"] != "dev":
		t.Fatalf("default: code %d result %v owner %v\nstderr: %s", code, result["status"], o, stderr)
	}

	stdout, stderr, code = runVerifyCLIIn(t, bin, dir, runtimeDir, "verify", "--json", "--allow-cross-session", "--", "true")
	result = decodeOne(t, stdout)
	if o := owner(result); code != 0 || result["status"] != "passed" || (attributed && o["scope"] != "cross_session") {
		t.Fatalf("--allow-cross-session: code %d result %v owner %v\nstderr: %s", code, result["status"], o, stderr)
	}

	stdout, _, code = runVerifyCLIIn(t, bin, dir, runtimeDir, "verify", "--allow-cross-session", "--", "true")
	if code != 0 || (attributed && (!strings.Contains(stdout, `another live session's answer: PID`) || !strings.Contains(stdout, `on a port leased by session "dev" (web.http, in /work/dev)`))) {
		t.Fatalf("human --allow-cross-session: code %d\n%s", code, stdout)
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

// TestCompareHumanRenderer_DereferencesPointerValues pins the human compare
// output: a difference's values are printed as values, not as heap
// addresses, and a null verdict is stated instead of omitted. The JSON path
// is untouched: it encodes the pointer itself.
func TestCompareHumanRenderer_DereferencesPointerValues(t *testing.T) {
	exitCode, other := 0, 1
	comparison := &verify.Comparison{
		SchemaVersion: verify.SchemaVersion, Kind: "verify.comparison", Status: "comparable", ExitCode: 0,
		OutcomesEqual: nil,
		Differences: []verify.Difference{
			{Field: "status", Baseline: "passed", Candidate: "failed"},
			{Field: "check.exit_code", Baseline: &exitCode, Candidate: &other},
			{Field: "check.stdout_sha256", Baseline: nil, Candidate: nil},
		},
		Sides: map[string]verify.SideSummary{}, Order: []string{},
		Limitations: []string{"output_truncated"}, Errors: []verify.ErrorRecord{},
	}
	var human bytes.Buffer
	if err := renderComparison(&human, comparison, false); err != nil {
		t.Fatal(err)
	}
	out := human.String()
	if !strings.Contains(out, "differs: check.exit_code baseline=0 candidate=1") {
		t.Fatalf("human output = %q, want the exit codes as numbers", out)
	}
	if strings.Contains(out, "0x") {
		t.Fatalf("human output = %q, want no pointer address", out)
	}
	if !strings.Contains(out, "differs: status baseline=passed candidate=failed") {
		t.Fatalf("human output = %q, want the string values unchanged", out)
	}
	if !strings.Contains(out, "differs: check.stdout_sha256 baseline=(none) candidate=(none)") {
		t.Fatalf("human output = %q, want a readable placeholder for a missing value", out)
	}
	if !strings.Contains(out, "outcomes equal: undetermined") {
		t.Fatalf("human output = %q, want the null verdict stated", out)
	}
	var machine bytes.Buffer
	if err := renderComparison(&machine, comparison, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine.String(), `{"field":"check.exit_code","baseline":0,"candidate":1}`) {
		t.Fatalf("json output = %q", machine.String())
	}
	if !strings.Contains(machine.String(), `"outcomes_equal":null`) {
		t.Fatalf("json output = %q", machine.String())
	}
}

// TestVerifyHumanRenderer_SortsEndpoints pins the human verify output: the
// endpoint lines are sorted by name, so the rendering is reproducible. The
// JSON path is already sorted by encoding/json and is unchanged.
func TestVerifyHumanRenderer_SortsEndpoints(t *testing.T) {
	result := &verify.Result{
		Session: "s", Status: "passed", ExitCode: 0,
		ConfigPath: "/x/proc-compose.yml",
		Startup:    verify.Startup{Status: "passed", Validation: "passed"},
		Check:      verify.Check{Status: "passed", Argv: []string{"true"}},
		Cleanup:    verify.Cleanup{Status: "passed", SignalingAuthority: "none"},
		Endpoints: map[string]verify.Endpoint{
			"db.http":   {Port: 5432, URL: "http://127.0.0.1:5432"},
			"api.http":  {Port: 3000, URL: "http://127.0.0.1:3000"},
			"api.admin": {Port: 3001, URL: "http://127.0.0.1:3001"},
		},
	}
	first := ""
	for i := 0; i < 40; i++ {
		var buf bytes.Buffer
		printVerifyHuman(&buf, result)
		if i == 0 {
			first = buf.String()
			continue
		}
		if buf.String() != first {
			t.Fatalf("rendering %d differs from the first:\n%s\n%s", i, first, buf.String())
		}
	}
	var got []string
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "  endpoint: ") {
			got = append(got, strings.Fields(strings.TrimPrefix(line, "  endpoint: "))[0])
		}
	}
	if strings.Join(got, ",") != "api.admin,api.http,db.http" {
		t.Fatalf("endpoints = %v, want them sorted by name", got)
	}
}

// TestAttemptsListing_SkipsInvalidReceipts pins that a receipt which does
// not satisfy the receipt contract is skipped by the attempts listing
// instead of failing it, and is never consumed as an attempt.
func TestAttemptsListing_SkipsInvalidReceipts(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	config := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(config, "invalid-receipt")
	if err != nil {
		t.Fatal(err)
	}
	dir := verify.AttemptsDir(paths.SessionDir(hash))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"schema_version":1,"kind":"verify.receipt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := listAttempts(hash, false)
	if err != nil {
		t.Fatalf("listAttempts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want the invalid receipt skipped", rows)
	}
}
