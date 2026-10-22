package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
)

func TestDiscoverSessions_ReadOnlyHandoffView(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	config := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(config, "feature x")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROC_COMPOSE_OWNER", "agent-7")
	project := &ipc.Project{Session: "feature x", RunID: "r1", ConfigPath: config, URLs: map[string]map[string]string{"api": {"http": "http://127.0.0.1:1"}}, Owner: session.NewOwner("up", config)}
	if err := session.Save(paths.Metadata(hash), project); err != nil {
		t.Fatal(err)
	}
	views, err := discoverSessions()
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %+v err %v", views, err)
	}
	if v := views[0]; v.Live || v.Stop != nil || v.Owner.Label != "agent-7" || v.Owner.Command != "up" || v.URLs["api"]["http"] == "" {
		t.Fatalf("stopped view = %+v", v)
	}
	if err := daemon.WritePID(paths.PID(hash), os.Getpid(), paths.Socket(hash)); err != nil {
		t.Fatal(err)
	}
	views, _ = discoverSessions()
	v := views[0]
	if !v.Live || !v.Retained || v.Stop == nil || !strings.Contains(*v.Stop, "--session 'feature x' stop") {
		t.Fatalf("live view = %+v stop=%v", v, v.Stop)
	}
	// Discovery must not take the session lease.
	if held, _ := session.InstanceHeld(hash); held {
		t.Fatal("discovery left a lease held")
	}
}

// TestDiscoverSessions_NeverProbesLease proves discovery stays read-only: live,
// unknown, and gone owners are classified without reaching the lease probe,
// which can momentarily acquire a free lease. Explicit inspection probes only
// the positively gone owner.
func TestDiscoverSessions_NeverProbesLease(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	config := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(config, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Save(paths.Metadata(hash), &ipc.Project{Session: "probe", RunID: "r1", ConfigPath: config}); err != nil {
		t.Fatal(err)
	}
	// The session PID file names this live process as the runner.
	if err := daemon.WritePID(paths.PID(hash), os.Getpid(), paths.Socket(hash)); err != nil {
		t.Fatal(err)
	}
	reaped := exec.Command(os.Args[0], "-test.run=^$")
	if err := reaped.Run(); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	owners := map[string]verify.Owner{
		"live":    {PID: os.Getpid(), Hostname: hostname},
		"unknown": {PID: os.Getpid(), Hostname: hostname + "-elsewhere"},
		"gone":    {PID: reaped.Process.Pid, Hostname: hostname},
	}
	want := map[string]string{}
	base := time.Now()
	for i, name := range []string{"live", "unknown", "gone"} {
		id, err := verify.NewAttemptID(base.Add(time.Duration(i) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		owner := owners[name]
		// The status is part of the receipt contract: an active receipt
		// carries not_run, the status of an attempt that has no verdict yet.
		receipt := &verify.Result{SchemaVersion: verify.SchemaVersion, State: verify.StateActive, Status: verify.StatusNotRun, AttemptID: &id, Owner: &owner}
		if err := verify.WriteReceipt(filepath.Join(verify.AttemptsDir(paths.SessionDir(hash)), id+".json"), receipt); err != nil {
			t.Fatal(err)
		}
		want[id] = name
	}
	probes := 0
	original := instanceHeld
	instanceHeld = func(string) (bool, error) { probes++; return false, nil }
	t.Cleanup(func() { instanceHeld = original })

	views, err := discoverSessions()
	if err != nil || len(views) != 1 || views[0].LatestAttempt == nil {
		t.Fatalf("views = %+v err %v", views, err)
	}
	rows, err := listAttempts(hash, false)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v err %v", rows, err)
	}
	if probes != 0 {
		t.Fatalf("read-only discovery probed the lease %d times", probes)
	}
	expected := map[string]string{"live": verify.StateActive, "unknown": verify.StateIndeterminate, "gone": verify.StateIndeterminate}
	for _, row := range rows {
		if got := row.State; got != expected[want[row.AttemptID]] {
			t.Errorf("discovery: %s owner = %s, want %s", want[row.AttemptID], got, expected[want[row.AttemptID]])
		}
	}

	rows, err = listAttempts(hash, true)
	if err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("explicit inspection probed %d times, want once for the gone owner", probes)
	}
	expected["gone"] = verify.StateAbandoned
	for _, row := range rows {
		if got := row.State; got != expected[want[row.AttemptID]] {
			t.Errorf("explicit: %s owner = %s, want %s", want[row.AttemptID], got, expected[want[row.AttemptID]])
		}
	}
}
