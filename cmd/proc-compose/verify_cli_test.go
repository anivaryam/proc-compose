package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestCLI_InvalidSessionIsRefusedTheSameWayEverywhere drives the real entry
// point over every command that accepts --json. The root pre-run hook refuses
// an invalid --session before the command does anything, and every command
// reports it the same way: exit status 2 and, with --json, exactly one JSON
// object on stdout. verify and compare render their own result kinds; every
// other command renders {operation, status: "error", message}. Without
// --json the exit status is the same and the error is on stderr.
func TestCLI_InvalidSessionIsRefusedTheSameWayEverywhere(t *testing.T) {
	bin := buildCLI(t)
	// After the build, which needs the real Go caches: the runs get a
	// throwaway home.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// No config exists, so a command that wrongly accepted the session could
	// not start or change anything.
	dir := t.TempDir()
	config := filepath.Join(dir, "proc-compose.yml")
	commands := []struct {
		operation string   // the command path below the root
		args      []string // the command's own arguments
		check     []string // the check after --, for verify and compare
	}{
		{operation: "status"},
		{operation: "logs", args: []string{"api"}},
		{operation: "why", args: []string{"api"}},
		{operation: "sessions"},
		{operation: "config check"},
		{operation: "config plan"},
		{operation: "doctor"},
		{operation: "bootstrap"},
		{operation: "attempts"},
		{operation: "up", args: []string{"--silent"}},
		{operation: "restart", args: []string{"api"}},
		{operation: "reload"},
		{operation: "verify", check: []string{"true"}},
		{operation: "compare", args: []string{"--baseline", dir, "--candidate", dir}, check: []string{"true"}},
	}

	// The table must cover every command with a --json option. The generated
	// man pages list each command's own options, so a new --json command
	// that is missing from the table fails here.
	manDir := t.TempDir()
	if stdout, stderr, code := runVerifyCLI(t, bin, dir, "man", "--dir", manDir); code != 0 {
		t.Fatalf("man: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	covered := map[string]bool{}
	for _, tc := range commands {
		covered["proc-compose-"+strings.ReplaceAll(tc.operation, " ", "-")+".1"] = true
	}
	pages, err := os.ReadDir(manDir)
	if err != nil {
		t.Fatal(err)
	}
	withJSON := 0
	for _, page := range pages {
		data, err := os.ReadFile(filepath.Join(manDir, page.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `\fB--json\fP`) {
			continue
		}
		withJSON++
		if !covered[page.Name()] {
			t.Errorf("%s has a --json option but is not in the table", page.Name())
		}
	}
	if withJSON != len(commands) {
		t.Errorf("%d man pages have a --json option, the table has %d commands", withJSON, len(commands))
	}

	for _, tc := range commands {
		t.Run(strings.ReplaceAll(tc.operation, " ", "_"), func(t *testing.T) {
			human := append([]string{"-f", config, "--session", "invalid space"}, strings.Fields(tc.operation)...)
			human = append(human, tc.args...)
			machine := append(append([]string{}, human...), "--json")
			if tc.check != nil {
				human = append(append(human, "--"), tc.check...)
				machine = append(append(machine, "--"), tc.check...)
			}

			stdout, stderr, code := runVerifyCLI(t, bin, dir, machine...)
			if code != 2 {
				t.Fatalf("--json: exit %d, want 2; stdout %q stderr %q", code, stdout, stderr)
			}
			result := decodeOne(t, stdout)
			switch tc.operation {
			case "verify":
				if result["kind"] != "verify.response" || result["exit_code"] != float64(2) || result["attempt_id"] != nil || result["config_path"] != config {
					t.Fatalf("verify --json result %v", result)
				}
			case "compare":
				if result["kind"] != "verify.comparison" || result["status"] != "not_comparable" || result["exit_code"] != float64(2) {
					t.Fatalf("compare --json result %v", result)
				}
			default:
				message, _ := result["message"].(string)
				want := map[string]any{"operation": tc.operation, "status": "error", "message": message}
				if !reflect.DeepEqual(result, want) || !strings.HasPrefix(message, "invalid --session: ") {
					t.Fatalf("--json result %v, want the refusal for %q", result, tc.operation)
				}
				if stderr != "" {
					t.Fatalf("--json stderr = %q, want the refusal on stdout only", stderr)
				}
			}
			if tc.check != nil {
				if errs, _ := result["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["code"] != "usage_error" {
					t.Fatalf("%s --json errors = %v", tc.operation, result["errors"])
				}
			}

			stdout, stderr, code = runVerifyCLI(t, bin, dir, human...)
			if code != 2 || !strings.Contains(stderr, "invalid --session") {
				t.Fatalf("human: exit %d, want 2; stdout %q stderr %q", code, stdout, stderr)
			}
			switch tc.operation {
			case "verify":
				if !strings.Contains(stdout, "verify FAILED (exit 2)") {
					t.Fatalf("verify human stdout = %q", stdout)
				}
			case "compare":
				if !strings.Contains(stdout, "compare NOT_COMPARABLE (exit 2)") {
					t.Fatalf("compare human stdout = %q", stdout)
				}
			default:
				if stdout != "" {
					t.Fatalf("human stdout = %q, want the refusal on stderr only", stdout)
				}
			}
		})
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

// TestLoadAttemptLatest_SkipsInvalidReceipts pins that `attempts latest` and
// `why --attempt latest`, which both resolve through loadAttempt, take the
// newest receipt the listing shows: a newer file that is not a receipt — a
// non-ULID name that sorts before every attempt id, or a ULID-named record
// with no attempt id — is skipped instead of failing the command. Naming such
// a file explicitly still fails.
func TestLoadAttemptLatest_SkipsInvalidReceipts(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	config := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(config, "latest-receipt")
	if err != nil {
		t.Fatal(err)
	}
	dir := verify.AttemptsDir(paths.SessionDir(hash))
	now := time.Now()
	id, err := verify.NewAttemptID(now)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &verify.Result{SchemaVersion: verify.SchemaVersion, AttemptID: &id, State: verify.StateCompleted, Status: verify.StatusPassed, ExitCode: verify.ExitPassed}
	if err := verify.WriteReceipt(filepath.Join(dir, id+".json"), receipt); err != nil {
		t.Fatal(err)
	}
	newer, err := verify.NewAttemptID(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"bad.json":      `{"schema_version":1,"kind":"verify.receipt"}`,
		newer + ".json": `{"schema_version":1,"kind":"verify.receipt","state":"completed","status":"passed"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"latest", ""} {
		got, path, err := loadAttempt(hash, ref)
		if err != nil {
			t.Fatalf("loadAttempt(%q): %v", ref, err)
		}
		if got.AttemptID == nil || *got.AttemptID != id || path != filepath.Join(dir, id+".json") {
			t.Fatalf("loadAttempt(%q) = %v at %s, want %s", ref, got.AttemptID, path, id)
		}
	}
	if _, _, err := loadAttempt(hash, newer); err == nil || !strings.Contains(err.Error(), "attempt_id") {
		t.Fatalf("loadAttempt(%s) = %v, want the invalid receipt refused", newer, err)
	}
}
