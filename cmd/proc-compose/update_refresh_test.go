package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/update"
)

// The release lookup the update notice schedules must survive the command that
// scheduled it. The notice runs in PersistentPreRunE and the lookup runs in the
// background, so a short command used to exit before the round trip finished
// and nothing was ever recorded.
//
// These tests drive the built binary under a real terminal, because the notice
// is only eligible when stderr is a terminal (stderrIsTerminal). The lookup
// itself is pointed at a local server that stalls and then fails, so the test
// needs no network and still discriminates: without the join at the end of the
// run, the process exits while the lookup is still in flight and nothing is
// recorded.

// stallingProxy starts a local HTTP proxy that accepts a connection, holds it
// for stall, and then closes it without answering. A client that uses it as
// its HTTPS proxy fails its request after about stall, without touching the
// network. Connections still held when the test ends are closed then.
func stallingProxy(t *testing.T, stall time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				select {
				case <-time.After(stall):
				case <-done:
				}
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

// updateRefreshEnv builds the environment for a run whose release lookup must
// fail after a short stall: a private cache directory, and a proxy that never
// answers. It returns the environment and the cache directory the run must
// write into.
func updateRefreshEnv(t *testing.T, stall time.Duration) (env []string, cacheDir string) {
	t.Helper()
	cacheRoot := t.TempDir()
	proxy := stallingProxy(t, stall)
	env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_CACHE_HOME="+cacheRoot,
		"HTTPS_PROXY="+proxy,
		"https_proxy="+proxy,
		"NO_PROXY=",
		"no_proxy=",
		"NO_COLOR=1",
	)
	return env, filepath.Join(cacheRoot, "proc-compose")
}

// recordedLookupOutcome fails the test unless the run that started at started
// recorded the outcome of its release lookup. The attempt is recorded before
// the lookup starts, so a recorded file alone does not prove the join: the
// outcome is stamped after the proxy's stall, the scheduled attempt before it.
func recordedLookupOutcome(t *testing.T, cacheDir string, started time.Time, stall time.Duration) {
	t.Helper()
	ok, checkedAt := recordedUpdateCache(t, cacheDir)
	if !ok {
		t.Fatal("the command ended without recording the release lookup")
	}
	if checkedAt.Before(started.Add(stall / 2)) {
		t.Fatalf("recorded attempt stamped %s after the run started, want the lookup's outcome (after the %s stall)", checkedAt.Sub(started), stall)
	}
}

// recordedUpdateCache reports whether the run recorded a release lookup, and
// returns the recorded attempt.
func recordedUpdateCache(t *testing.T, cacheDir string) (bool, time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cacheDir, update.CacheFileName))
	if err != nil {
		return false, time.Time{}
	}
	var entry struct {
		Latest    string    `json:"latest"`
		CheckedAt time.Time `json:"checked_at"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("cache file is not valid JSON: %v\n%s", err, data)
	}
	if entry.CheckedAt.IsZero() {
		t.Fatalf("cache file has no attempt stamp: %s", data)
	}
	return true, entry.CheckedAt
}

// TestUpdateRefresh_ShortCommandRecordsTheCache is the end-to-end proof for
// the success path: a short, eligible command ends with the release lookup
// recorded, so the next invocation can announce it.
func TestUpdateRefresh_ShortCommandRecordsTheCache(t *testing.T) {
	bin := buildTestBinaryVersion(t, "v1.0.0")
	const stall = 700 * time.Millisecond
	env, cacheDir := updateRefreshEnv(t, stall)
	work := t.TempDir()
	writeCLIFile(t, filepath.Join(work, "proc-compose.yml"), "processes: {app: {cmd: echo hello}}\n")

	started := time.Now()
	out := runInteractive(t, bin, work, env, true, "list")
	if strings.Contains(out, "new proc-compose release") {
		t.Errorf("a failed lookup announced a release:\n%s", out)
	}
	recordedLookupOutcome(t, cacheDir, started, stall)
}

// TestUpdateRefresh_StalledLookupIsWaitedForOnce is the stalled release API:
// the proxy accepts and never answers within the run, so the join gives up at
// its budget. That run must still record the attempt, and the next run must
// not wait again; otherwise every command would pay the budget.
func TestUpdateRefresh_StalledLookupIsWaitedForOnce(t *testing.T) {
	bin := buildTestBinaryVersion(t, "v1.0.0")
	env, cacheDir := updateRefreshEnv(t, time.Minute)
	work := t.TempDir()
	writeCLIFile(t, filepath.Join(work, "proc-compose.yml"), "processes: {app: {cmd: echo hello}}\n")

	started := time.Now()
	runInteractive(t, bin, work, env, true, "list")
	first := time.Since(started)
	if ok, _ := recordedUpdateCache(t, cacheDir); !ok {
		t.Fatalf("a run whose lookup stalled (%s) recorded nothing, so the next run waits again", first)
	}

	started = time.Now()
	runInteractive(t, bin, work, env, true, "list")
	if second := time.Since(started); second >= updateRefreshBudget/2 {
		t.Fatalf("the run after a stalled lookup took %s (the first took %s), want no second wait for the %s budget", second, first, updateRefreshBudget)
	}
}

// TestUpdateRefresh_FailedCommandRecordsTheCache is the end-to-end proof for
// the error path: cobra returns before PostRun when a command's RunE fails,
// and the CLI exits, so the join has to happen before that exit. A command
// that fails after the notice ran must still record the lookup.
func TestUpdateRefresh_FailedCommandRecordsTheCache(t *testing.T) {
	bin := buildTestBinaryVersion(t, "v1.0.0")
	const stall = 700 * time.Millisecond
	env, cacheDir := updateRefreshEnv(t, stall)
	work := t.TempDir()
	// A config the loader refuses: the command fails after PersistentPreRunE
	// has already scheduled the lookup.
	writeCLIFile(t, filepath.Join(work, "proc-compose.yml"), "processes: {app: {cmd: echo hello}\n")

	started := time.Now()
	out, err := runInteractiveExit(t, bin, work, env, true, "up")
	if err == nil {
		t.Fatalf("a broken config did not fail the command:\n%s", out)
	}
	recordedLookupOutcome(t, cacheDir, started, stall)
}

// TestUpdateRefresh_NonInteractiveRunDoesNotRecordTheCache pins the
// eligibility rule: a run whose stderr is not a terminal never schedules the
// lookup, so it never waits for one either.
func TestUpdateRefresh_NonInteractiveRunDoesNotRecordTheCache(t *testing.T) {
	bin := buildTestBinaryVersion(t, "v1.0.0")
	env, cacheDir := updateRefreshEnv(t, 700*time.Millisecond)
	work := t.TempDir()
	writeCLIFile(t, filepath.Join(work, "proc-compose.yml"), "processes: {app: {cmd: echo hello}}\n")

	cmd := exec.Command(bin, "list")
	cmd.Dir = work
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	if ok, _ := recordedUpdateCache(t, cacheDir); ok {
		t.Fatal("a run without a terminal recorded a release lookup")
	}
}
