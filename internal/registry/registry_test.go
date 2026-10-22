package registry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
)

// saveSession writes metadata for a session named name and returns its hash.
func saveSession(t *testing.T, name string, ports map[string]map[string]int) string {
	t.Helper()
	config := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, err := paths.FromConfig(config, name)
	if err != nil {
		t.Fatal(err)
	}
	project := &ipc.Project{Session: name, RunID: "run-" + name, ConfigPath: config, Ports: ports}
	if err := session.Save(paths.Metadata(hash), project); err != nil {
		t.Fatal(err)
	}
	return hash
}

// exitedPID returns the PID of a process that has already exited.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func byName(sessions []Session) map[string]Session {
	out := map[string]Session{}
	for _, s := range sessions {
		out[s.Name()] = s
	}
	return out
}

// TestDiscover_ReadsMetadataRecordAndLiveness pins what the registry reads:
// every readable metadata file, liveness from the PID file re-validated by
// start time, and the PID file record with its launched groups. Unreadable
// metadata is skipped, and nothing takes a lease.
func TestDiscover_ReadsMetadataRecordAndLiveness(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	live := saveSession(t, "live", map[string]map[string]int{"api": {"http": 4100}})
	if err := daemon.WritePID(paths.PID(live), os.Getpid(), paths.Socket(live)); err != nil {
		t.Fatal(err)
	}
	groups := []daemon.Group{{Name: "api", PGID: 4242, LeaderStart: 7}}
	if err := daemon.WriteGroups(paths.PID(live), os.Getpid(), groups); err != nil {
		t.Fatal(err)
	}
	dead := saveSession(t, "dead", nil)
	if err := daemon.WritePID(paths.PID(dead), exitedPID(t), paths.Socket(dead)); err != nil {
		t.Fatal(err)
	}
	reused := saveSession(t, "reused", nil)
	self, err := daemon.ReadRecord(paths.PID(live))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(daemon.Record{PID: os.Getpid(), StartedAt: self.StartedAt + 1})
	if err := daemon.WriteFileAtomic(paths.PID(reused), data, 0o600); err != nil {
		t.Fatal(err)
	}
	saveSession(t, "", nil) // metadata only: never started, or its PID file is gone
	if err := os.WriteFile(paths.Metadata("badbadba"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 4 {
		t.Fatalf("Discover = %d sessions, want 4 (unreadable metadata skipped): %+v", len(sessions), sessions)
	}
	for i := 1; i < len(sessions); i++ {
		a, b := sessions[i-1], sessions[i]
		if a.Project.ConfigPath > b.Project.ConfigPath || (a.Project.ConfigPath == b.Project.ConfigPath && a.Name() > b.Name()) {
			t.Fatalf("sessions not sorted by config path and name: %+v", sessions)
		}
	}
	got := byName(sessions)

	s := got["live"]
	if s.Hash != live || !s.Live || s.PID != os.Getpid() || s.Record == nil || s.Record.Groups == nil || len(*s.Record.Groups) != 1 || (*s.Record.Groups)[0] != groups[0] {
		t.Fatalf("live session = %+v (record %+v)", s, s.Record)
	}
	if proven := daemon.StartProven(*s.Record); proven != (s.Record.StartedAt != 0) {
		t.Fatalf("StartProven(live record) = %v with start time %d", proven, s.Record.StartedAt)
	}
	if s := got["dead"]; s.Live || s.PID != 0 || s.Record == nil || daemon.StartProven(*s.Record) {
		t.Fatalf("dead session = %+v", s)
	}
	if s := got["reused"]; s.Record == nil || daemon.StartProven(*s.Record) {
		t.Fatalf("reused-pid session = %+v, want a record that is not proven", s)
	} else if self.StartedAt != 0 && s.Live {
		t.Fatalf("reused-pid session reads live: %+v", s)
	}
	if s := got["default"]; s.Live || s.Record != nil || s.Project.Session != "" {
		t.Fatalf("metadata-only session = %+v", s)
	}
	if runtime.GOOS != "darwin" && self.StartedAt == 0 {
		t.Fatalf("no start time recorded on %s", runtime.GOOS)
	}
}

// TestDiscover_SkipsMetadataThatIsNoSession pins that a metadata file which
// does not describe a session is skipped: a corrupt or foreign document never
// registers a phantom session.
func TestDiscover_SkipsMetadataThatIsNoSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for name, body := range map[string]string{
		"pc-null.meta.json":    "null",
		"pc-empty.meta.json":   "{}",
		"pc-garbage.meta.json": "{",
	} {
		if err := os.WriteFile(paths.Metadata(strings.TrimSuffix(strings.TrimPrefix(name, "pc-"), ".meta.json")), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("Discover = %d sessions, want 0: %+v", len(sessions), sessions)
	}
}

// TestDiscover_EmptyRuntimeDirectory lists nothing, as an empty list.
func TestDiscover_EmptyRuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	sessions, err := Discover()
	if err != nil || sessions == nil || len(sessions) != 0 {
		t.Fatalf("Discover = %v, %v; want an empty list", sessions, err)
	}
}

// TestPortAssignment finds the process and named port a port is assigned to.
func TestPortAssignment(t *testing.T) {
	s := Session{Project: &ipc.Project{Ports: map[string]map[string]int{"api": {"http": 4100, "admin": 4101}, "web": {"http": 4200}}}}
	for port, want := range map[int][2]string{4100: {"api", "http"}, 4101: {"api", "admin"}, 4200: {"web", "http"}} {
		if process, name, ok := s.PortAssignment(port); !ok || process != want[0] || name != want[1] {
			t.Errorf("PortAssignment(%d) = %q, %q, %v; want %v", port, process, name, ok, want)
		}
	}
	for _, port := range []int{0, 4300} {
		if _, _, ok := s.PortAssignment(port); ok {
			t.Errorf("PortAssignment(%d) found an assignment", port)
		}
	}
}
