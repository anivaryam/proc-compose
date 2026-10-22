package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestStripFlag_NoValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		flag string
		want []string
	}{
		{"single hit", []string{"a", "--silent", "b"}, "--silent", []string{"a", "b"}},
		{"start", []string{"--silent", "a"}, "--silent", []string{"a"}},
		{"end", []string{"a", "--silent"}, "--silent", []string{"a"}},
		{"multiple", []string{"--silent", "a", "--silent"}, "--silent", []string{"a"}},
		{"absent", []string{"a", "b"}, "--silent", []string{"a", "b"}},
		{"empty", []string{}, "--silent", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripFlag(tc.in, tc.flag, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripFlag(%v, %q, false) = %v, want %v", tc.in, tc.flag, got, tc.want)
			}
		})
	}
}

func TestStripFlag_WithValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		flag string
		want []string
	}{
		{"single", []string{"-f", "config.yml", "up"}, "-f", []string{"up"}},
		{"middle", []string{"up", "--file", "x.yml", "--name", "n"}, "--file", []string{"up", "--name", "n"}},
		{"end", []string{"up", "--file", "x.yml"}, "--file", []string{"up"}},
		{"absent", []string{"up", "--silent"}, "--file", []string{"up", "--silent"}},
		{"two occurrences", []string{"--file", "a", "--file", "b"}, "--file", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripFlag(tc.in, tc.flag, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripFlag(%v, %q, true) = %v, want %v", tc.in, tc.flag, got, tc.want)
			}
		})
	}
}

func TestPIDRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePID(path, 4242, "/tmp/test.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}

	pid, addr, err := ReadPID(path)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 4242 {
		t.Errorf("pid = %d, want 4242", pid)
	}
	if addr != "/tmp/test.sock" {
		t.Errorf("addr = %q, want %q", addr, "/tmp/test.sock")
	}
}

func TestReadPID_Missing(t *testing.T) {
	_, _, err := ReadPID(filepath.Join(t.TempDir(), "missing.pid"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestReadPID_Corrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pid")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadPID(path); err == nil {
		t.Fatal("expected error for corrupted PID file, got nil")
	}
}

func TestCleanup_RemovesFiles(t *testing.T) {
	dir := t.TempDir()
	pid := filepath.Join(dir, "x.pid")
	sock := filepath.Join(dir, "x.sock")
	os.WriteFile(pid, []byte("{}"), 0600)
	os.WriteFile(sock, []byte(""), 0600)

	Cleanup(pid, sock)

	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Error("pid file should be removed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Error("socket file should be removed")
	}
}

func TestCleanup_MissingFilesNoError(t *testing.T) {
	// Cleanup is best-effort; should not panic on missing files.
	Cleanup("/nonexistent.pid", "/nonexistent.sock")
}

func TestPIDFile_Mode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if mode != 0600 {
		t.Errorf("PID file mode = %o, want 0600 (world-readable PID file leaks runtime path)", mode)
	}
}

// TestIsAliveFromPIDFile_RoundtripSelf verifies that a pidfile written for
// the current process reports alive (the start-time check matches because
// the OS hasn't reused our PID).
func TestIsAliveFromPIDFile_RoundtripSelf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self.pid")
	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	alive, pid := IsAliveFromPIDFile(path)
	if !alive {
		t.Errorf("expected current process to be alive, got false (pid=%d)", pid)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

// TestIsAliveFromPIDFile_MissingFile returns alive=false, pid=0 cleanly.
func TestIsAliveFromPIDFile_MissingFile(t *testing.T) {
	alive, pid := IsAliveFromPIDFile(filepath.Join(t.TempDir(), "nope.pid"))
	if alive {
		t.Error("expected alive=false for missing pidfile")
	}
	if pid != 0 {
		t.Errorf("expected pid=0 for missing pidfile, got %d", pid)
	}
}

// TestIsAliveFromPIDFile_StartTimeMismatch ensures that when the recorded
// start-time disagrees with the live process's start-time we treat the
// pidfile as stale (defending against PID reuse).
func TestIsAliveFromPIDFile_StartTimeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")

	// Synthesize a pidfile that points at *our* PID but with a wrong
	// start-time. On Linux procStartTime returns a real value; on other
	// Unixes it returns 0 and we fall back to liveness-only — skip there.
	startedAt, err := procStartTime(os.Getpid())
	if err != nil {
		t.Skipf("procStartTime unavailable: %v", err)
	}
	if startedAt == 0 {
		t.Skip("procStartTime unavailable on this platform")
	}
	stale := Record{PID: os.Getpid(), Addr: "/tmp/x.sock", StartedAt: 1}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	alive, pid := IsAliveFromPIDFile(path)
	if alive {
		t.Errorf("expected stale=true to surface as alive=false, got alive=true (pid=%d)", pid)
	}
}

// TestStartProven_NeverFallsBackToThePID pins the strict liveness check:
// only a live pid with the recorded start time is proven. A record without a
// start time, which IsAliveFromPIDFile accepts on the pid alone, is not.
func TestStartProven_NeverFallsBackToThePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "self.pid")
	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.StartedAt == 0 {
		if StartProven(rec) {
			t.Fatal("a record without a start time was proven")
		}
		t.Skip("no process start time on " + runtime.GOOS)
	}
	if !StartProven(rec) {
		t.Fatalf("StartProven(%+v) = false for this live process", rec)
	}
	for name, stale := range map[string]Record{
		"start time mismatch": {PID: rec.PID, StartedAt: rec.StartedAt + 1},
		"no start time":       {PID: rec.PID},
		"no pid":              {StartedAt: rec.StartedAt},
	} {
		if StartProven(stale) {
			t.Errorf("%s: StartProven(%+v) = true", name, stale)
		}
	}
	if alive, _ := IsAliveFromPIDFile(path); !alive {
		t.Fatal("IsAliveFromPIDFile disagrees for the same live record")
	}
}

// TestWriteFileAtomic_SyncsTheParentDirectory pins that the rename installs
// the file durably: the directory entry it creates is flushed, so a crash
// cannot lose a file that was reported as written.
func TestWriteFileAtomic_SyncsTheParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipt.json")

	var synced string
	old := syncDir
	syncDir = func(d string) error {
		synced = d
		return nil
	}
	defer func() { syncDir = old }()

	if err := WriteFileAtomic(path, []byte("{}"), 0600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if synced != dir {
		t.Fatalf("synced directory = %q, want %q", synced, dir)
	}
}

// TestWriteFileAtomic_DirectorySyncErrorDoesNotFailTheWrite pins that the
// durability step is best-effort: a filesystem that refuses to flush a
// directory (Windows, some network mounts) must not turn a successful write
// into a failed one.
func TestWriteFileAtomic_DirectorySyncErrorDoesNotFailTheWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipt.json")
	want := []byte(`{"pid":7}`)

	old := syncDir
	syncDir = func(string) error { return errors.New("directory sync unsupported") }
	defer func() { syncDir = old }()

	if err := WriteFileAtomic(path, want, 0600); err != nil {
		t.Fatalf("WriteFileAtomic = %v, want nil despite a failed directory sync", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file holds %q, want %q", got, want)
	}
}

// TestSyncDirectory_TempDirSucceeds proves the real implementation works on a
// directory this platform can flush, so the stubbed tests above exercise the
// seam and not a broken default.
func TestSyncDirectory_TempDirSucceeds(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncDirectory: %v", err)
	}
}

// TestWriteGroups_PreservesPIDAddrAndStart pins the record contract: writing
// the process-group record keeps the daemon's own identity intact, so liveness
// checks still recognise the daemon, and the atomic writer leaves no temporary
// file behind.
func TestWriteGroups_PreservesPIDAddrAndStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self.pid")
	if err := WritePID(path, os.Getpid(), "/tmp/x.sock"); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	before, err := ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord before: %v", err)
	}
	want := []Group{{Name: "web", PGID: 42, LeaderStart: 7}}
	if err := WriteGroups(path, os.Getpid(), want); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Groups == nil || !reflect.DeepEqual(*rec.Groups, want) {
		t.Fatalf("groups = %v, want %v", rec.Groups, want)
	}
	if rec.PID != os.Getpid() || rec.Addr != "/tmp/x.sock" || rec.StartedAt != before.StartedAt {
		t.Fatalf("record = %+v, want pid=%d addr=/tmp/x.sock started_at=%d", rec, os.Getpid(), before.StartedAt)
	}
	if rec.Host != HostIdentity() {
		t.Fatalf("host = %+v, want %+v", rec.Host, HostIdentity())
	}
	if alive, pid := IsAliveFromPIDFile(path); !alive || pid != os.Getpid() {
		t.Fatalf("IsAliveFromPIDFile = %v, %d after WriteGroups; want true, %d", alive, pid, os.Getpid())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "self.pid" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only self.pid (no temporary files)", names)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Fatalf("PID file mode after WriteGroups = %o, want 0600", mode)
		}
	}
}

// TestWriteGroups_EmptyListIsRecorded distinguishes "recorded, nothing live"
// from "no record at all": an empty list must survive the round trip.
func TestWriteGroups_EmptyListIsRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.pid")
	if err := WritePID(path, 4242, "/tmp/x.sock"); err != nil {
		t.Fatal(err)
	}
	if err := WriteGroups(path, 4242, nil); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Groups == nil || len(*rec.Groups) != 0 {
		t.Fatalf("groups = %v, want a recorded empty list", rec.Groups)
	}
}

// TestWriteGroups_MissingPIDFileFails ensures a record is never written into a
// PID file that no longer exists (for example after Cleanup).
func TestWriteGroups_MissingPIDFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.pid")
	if err := WriteGroups(path, os.Getpid(), []Group{{Name: "web"}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteGroups on a missing PID file = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("WriteGroups created %s: %v", path, err)
	}
}

// TestWriteGroups_OtherOwnerIsRefused: a PID file that now names another
// daemon (one that recreated it after this daemon's file was deleted) is left
// exactly as it is, so neither daemon's record replaces the other's.
func TestWriteGroups_OtherOwnerIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.pid")
	if err := WritePID(path, 4242, "/tmp/other.sock"); err != nil {
		t.Fatal(err)
	}
	if err := WriteGroups(path, 4242, []Group{{Name: "theirs", PGID: 4300}}); err != nil {
		t.Fatalf("WriteGroups by the owner: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = WriteGroups(path, 4243, []Group{{Name: "ours", PGID: 4400}})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("WriteGroups by another pid = %v, want ErrNotOwner", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused WriteGroups changed the file:\nbefore %s\nafter  %s", before, after)
	}
}

// TestReadRecord_LegacyFileHasNoGroups: a PID file written before group
// recording has nil groups, which is distinct from a recorded empty list.
func TestReadRecord_LegacyFileHasNoGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.pid")
	if err := os.WriteFile(path, []byte(`{"pid":900,"addr":"/tmp/x.sock","started_at":51234}`), 0600); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Groups != nil {
		t.Fatalf("legacy groups = %v, want nil", *rec.Groups)
	}
	if rec.PID != 900 || rec.Addr != "/tmp/x.sock" || rec.StartedAt != 51234 {
		t.Fatalf("legacy record = %+v", rec)
	}
	if err := os.WriteFile(path, []byte(`{"pid":900,"addr":"/tmp/x.sock","groups":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	rec, err = ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Groups == nil || len(*rec.Groups) != 0 {
		t.Fatalf("recorded empty groups = %v, want non-nil empty", rec.Groups)
	}
}

// TestReadRecord_CorruptIsError: an unreadable record is an error, never a
// legacy file with no groups.
func TestReadRecord_CorruptIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pid")
	if err := os.WriteFile(path, []byte(`{"pid":900,"groups":[{"name":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(path); err == nil {
		t.Fatal("ReadRecord accepted a corrupt PID file")
	}
}
