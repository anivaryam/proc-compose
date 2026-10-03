package update

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/ansi"
)

// cacheFile builds a Checker whose cache lives in a throwaway directory, and
// returns that path so a test can inspect or seed the record directly.
func testChecker(t *testing.T) (*Checker, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), CacheFileName)
	c := New(path)
	return c, path
}

// seedCache writes a cache entry directly, standing in for a lookup that
// already happened in some earlier invocation.
func seedCache(t *testing.T, path, latest string, checkedAt time.Time) {
	t.Helper()
	data, err := json.Marshal(cacheEntry{Latest: latest, CheckedAt: checkedAt})
	if err != nil {
		t.Fatalf("marshal cache entry: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

// readCacheFile returns the recorded entry, failing the test if the file is
// missing or malformed.
func readCacheFile(t *testing.T, path string) cacheEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("cache is not valid JSON (%v): %s", err, data)
	}
	return entry
}

// testServer serves body with the given status and records how many requests it
// received, so a test can prove a call was or was not made.
func testServer(t *testing.T, status int, body string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

func releaseJSON(tag string, draft, prerelease bool) string {
	return `{"tag_name":"` + tag + `","draft":` + boolJSON(draft) + `,"prerelease":` + boolJSON(prerelease) + `}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ── version comparison ───────────────────────────────────────────────────────

func TestNewer_ComparesSemantically(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		latest    string
		want      bool
	}{
		{"newer patch", "v1.2.0", "v1.3.0", true},
		{"newer minor", "v1.2.0", "v1.3.1", true},
		{"newer major", "v1.9.9", "v2.0.0", true},
		{"equal", "v1.2.0", "v1.2.0", false},
		{"older release", "v1.3.0", "v1.2.0", false},
		{"installed ahead", "v2.0.0", "v1.9.9", false},
		{"double digit minor is not read as text", "v1.9.0", "v1.10.0", true},
		{"double digit patch is not read as text", "v1.2.9", "v1.2.10", true},
		{"missing leading v on installed", "1.2.0", "v1.3.0", true},
		{"missing leading v on latest", "v1.2.0", "1.3.0", true},
		{"missing v on both", "1.2.0", "1.3.0", true},
		{"invalid installed leading zero", "v1.02.0", "v1.2.1", false},
		{"invalid release leading zero", "v1.2.0", "v1.02.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Newer(tt.installed, tt.latest); got != tt.want {
				t.Errorf("Newer(%q, %q) = %v, want %v", tt.installed, tt.latest, got, tt.want)
			}
		})
	}
}

// TestNewer_SkipsUnusableInstalledVersions covers the versions a local build
// can actually carry. `git describe` (Makefile) yields commit hashes and
// describe-suffixed tags; the Makefile falls back to "dev"; a dirty tree adds
// "-dirty". None of them describe a published release, so none may be told to
// upgrade.
func TestNewer_SkipsUnusableInstalledVersions(t *testing.T) {
	for _, installed := range []string{
		"dev",
		"",
		"abc1234",
		"v1.2.0-3-gabc1234", // git describe: commits past the tag
		"v1.2.0-dirty",      // git describe on a dirty tree
		"v1.2.0-rc1",        // prerelease
		"v1.2",              // incomplete
		"v1.2.3.4",          // too many components
		"vv1.2.3",
		"v1.2.x",
		"v1.-2.3",
		"v1.2.3 ",
	} {
		if Newer(installed, "v9.9.9") {
			t.Errorf("Newer(%q, v9.9.9) = true; development, prerelease and unparseable versions must be skipped", installed)
		}
	}
}

func TestNewer_SkipsUnusableReleaseTag(t *testing.T) {
	for _, latest := range []string{"", "nightly", "v1.2", "v1.2.3-rc1", "v1.2.3+build"} {
		if Newer("v1.0.0", latest) {
			t.Errorf("Newer(v1.0.0, %q) = true; an unparseable release tag must never trigger a notice", latest)
		}
	}
}

func TestParseVersion_RejectsNonNumericComponents(t *testing.T) {
	for _, s := range []string{"v1.two.3", "v+1.2.3", "v1.2.-3", "v 1.2.3", "v1..3"} {
		if _, ok := parseVersion(s); ok {
			t.Errorf("parseVersion(%q) accepted a non-numeric component", s)
		}
	}
}

// ── message rendering ────────────────────────────────────────────────────────

// TestMessageFor_DefaultInstallDirectoryIsNotAssumedToBeBrokitManaged is the
// originally reported case. The binary sits in the default install directory,
// which is also brokit's own default — the two are indistinguishable from in
// here, because this repository's install.sh and Makefile use the same one.
//
// Directory membership is therefore not evidence, and a copy there must still be
// given a command that works. Asserting the bare `brokit update` line for that
// directory is exactly the dead end this notice exists to remove.
func TestMessageFor_DefaultInstallDirectoryIsNotAssumedToBeBrokitManaged(t *testing.T) {
	noColour(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this on Windows
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	dir := defaultBinDir(home)

	got := MessageFor("v1.2.0", "v1.3.0", Location{Dir: dir})

	if want := "Run: " + UpdateCommand + "\n"; strings.Contains(got, want) {
		t.Errorf("a copy in the default install directory (%s) was given only the brokit command:\n%s", dir, got)
	}
	assertInPlaceCommand(t, got, dir)
}

// TestMessageFor_AlwaysOffersAnInPlaceUpdate: wherever the copy lives, the
// notice has to carry a command that writes to that directory.
func TestMessageFor_AlwaysOffersAnInPlaceUpdate(t *testing.T) {
	noColour(t)

	for _, dir := range []string{
		filepath.Join(t.TempDir(), ".local", "bin"),
		filepath.Join(t.TempDir(), "go", "bin"),
		filepath.Join(t.TempDir(), "usr", "local", "bin"),
	} {
		got := MessageFor("v1.2.0", "v1.3.0", Location{Dir: dir})

		if !strings.Contains(got, dir) {
			t.Errorf("notice does not name the running copy's directory %q:\n%s", dir, got)
		}
		if strings.Contains(got, "Run: "+UpdateCommand+"\n") {
			t.Errorf("notice in %q leads with a command that may not apply:\n%s", dir, got)
		}
		assertInPlaceCommand(t, got, dir)
	}
}

// defaultBinDir is brokit's documented default, computed here independently of
// the package so the test pins the behaviour rather than the implementation.
func defaultBinDir(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Local", "brokit", "bin")
	}
	return filepath.Join(home, ".local", "bin")
}

// TestMessageFor_NeverClaimsBrokitOwnership: the word "manage" may only appear
// as a condition brokit itself resolves, never as a statement about this copy.
func TestMessageFor_NeverClaimsBrokitOwnership(t *testing.T) {
	noColour(t)

	got := MessageFor("v1.2.0", "v1.3.0", Location{Dir: filepath.Join(t.TempDir(), ".local", "bin")})

	for _, forbidden := range []string{"which brokit manages", "is managed by brokit", "brokit-managed"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("notice asserts ownership with %q:\n%s", forbidden, got)
		}
	}
}

// TestMessageFor_PathWithSpacesIsQuoted: the commands are meant to be pasted, and
// an unquoted path silently targets the wrong directory.
func TestMessageFor_PathWithSpacesIsQuoted(t *testing.T) {
	noColour(t)
	dir := filepath.Join(t.TempDir(), "my tools")

	got := MessageFor("v1.2.0", "v1.3.0", Location{Dir: dir})

	assertInPlaceCommand(t, got, dir)
}

// TestMessageFor_UnknownLocationKeepsEstablishedGuidance: when the directory
// cannot be determined there is nothing to point a command at, so the notice
// stays as it was rather than inventing a path.
func TestMessageFor_UnknownLocationKeepsEstablishedGuidance(t *testing.T) {
	noColour(t)

	got := MessageFor("v1.2.0", "v1.3.0", Location{})

	if want := "Run: " + UpdateCommand; !strings.Contains(got, want) {
		t.Errorf("unknown location is missing %q:\n%s", want, got)
	}
	if strings.Contains(got, "PROC_COMPOSE_INSTALL_DIR") || strings.Contains(got, "BROKIT_BIN") {
		t.Errorf("unknown location must not print a directory it does not know:\n%s", got)
	}
}

// TestMessage_UsesColourWhenEnabled is the other half: the notice is styled
// like the rest of the CLI rather than silently losing its colour.
func TestMessage_UsesColourWhenEnabled(t *testing.T) {
	ansi.SetDisabled(false)
	if !strings.Contains(Message("v1.2.0", "v1.3.0"), ansi.Bold) {
		t.Error("message carries no bold styling when colour is enabled")
	}
}

// TestMessageFor_KeepsColour is the same guarantee on the branch that carries
// the most text.
func TestMessageFor_KeepsColour(t *testing.T) {
	ansi.SetDisabled(false)
	t.Cleanup(func() { ansi.SetDisabled(true) })

	got := MessageFor("v1.2.0", "v1.3.0", Location{Dir: "/opt/proc compose"})

	if !strings.Contains(got, ansi.Bold) {
		t.Errorf("notice carries no bold styling when colour is enabled:\n%q", got)
	}
}

// TestMessage_DetectsTheRunningTestBinary proves the entry point is wired to a
// real location rather than a placeholder.
func TestMessage_DetectsTheRunningTestBinary(t *testing.T) {
	noColour(t)

	loc := Detect()
	if loc.Dir == "" {
		t.Fatal("Detect returned no directory for the running test binary")
	}
	if !strings.Contains(Message("v1.2.0", "v1.3.0"), loc.Dir) {
		t.Errorf("notice does not name the directory of the running binary (%q)", loc.Dir)
	}
}

// assertInPlaceCommand checks that the notice carries a command which writes to
// dir, in this platform's shell syntax.
func assertInPlaceCommand(t *testing.T, got, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if !strings.Contains(got, `$env:BROKIT_BIN = '`+dir+`'; brokit install --force proc-compose`) {
			t.Errorf("notice lacks a PowerShell in-place command for %q:\n%s", dir, got)
		}
		if !strings.Contains(got, `[Environment]::SetEnvironmentVariable("BROKIT_BIN", '`+dir+`', "User")`) {
			t.Errorf("notice does not make the Windows override reach new terminals:\n%s", got)
		}
		return
	}
	if !strings.Contains(got, "PROC_COMPOSE_INSTALL_DIR=") {
		t.Errorf("notice does not aim the installer at the running copy's directory:\n%s", got)
	}
	if strings.Contains(got, "PROC_COMPOSE_INSTALL_DIR="+posixQuoteArg(dir)+" curl") {
		t.Errorf("the override is set for curl instead of the installer:\n%s", got)
	}
}

// ── platform rendering ───────────────────────────────────────────────────────

// TestPosixUpdateAdvice_AssignmentIsOnTheReceivingCommand: `VAR=x curl … | bash`
// sets the variable for curl. The installer on the other end of the pipe would
// fall back to its own default and update a different file than the one in use.
func TestPosixUpdateAdvice_AssignmentIsOnTheReceivingCommand(t *testing.T) {
	command, _ := posixUpdateAdvice("/opt/my tools")

	if !strings.Contains(command, "curl -sSfL "+InstallScriptURL+" | ") {
		t.Fatalf("command does not pipe the installer from the documented URL: %s", command)
	}
	if !strings.Contains(command, "| PROC_COMPOSE_INSTALL_DIR='/opt/my tools' bash") {
		t.Errorf("the assignment is not on the command that reads it: %s", command)
	}
}

func TestPosixUpdateAdvice_NamesBrokitWithoutAssertingOwnership(t *testing.T) {
	_, note := posixUpdateAdvice("/opt/bin")

	if !strings.Contains(note, UpdateCommand) {
		t.Errorf("note does not mention the brokit route: %s", note)
	}
	if !strings.Contains(note, "If brokit is not managing it yet") {
		t.Errorf("note must present brokit as a condition, not a fact: %s", note)
	}
}

// TestWindowsUpdateAdvice_UsesForceSoItWorksForBothCases: brokit refuses a
// plain `install` once a record exists, so a notice that only knew how to say
// "install" would be useless for a copy brokit already manages — which is
// exactly the case this notice cannot rule out. `--force` installs and records
// either way, so one command covers both.
func TestWindowsUpdateAdvice_UsesForceSoItWorksForBothCases(t *testing.T) {
	command, note := windowsUpdateAdvice(`C:\my tools`)

	if command != `$env:BROKIT_BIN = 'C:\my tools'; brokit install --force proc-compose` {
		t.Errorf("windows command is not PowerShell, or cannot upgrade a managed copy: %s", command)
	}
	for _, forbidden := range []string{"BROKIT_BIN=", "bash", "curl"} {
		if strings.Contains(command, forbidden) {
			t.Errorf("windows command contains %q: %s", forbidden, command)
		}
	}
	if !strings.Contains(note, `[Environment]::SetEnvironmentVariable("BROKIT_BIN", 'C:\my tools', "User")`) {
		t.Errorf("windows advice does not make the override reach new terminals: %s", note)
	}
	if !strings.Contains(note, "brokit update proc-compose") {
		t.Errorf("windows advice does not name the follow-up update: %s", note)
	}
}

func TestWindowsQuoteArg_DoublesSingleQuotes(t *testing.T) {
	// PowerShell treats single-quoted text as literal and escapes a quote by
	// doubling it, unlike the POSIX '\'' form.
	if got := windowsQuoteArg(`C:\O'Brien\tools`); got != `'C:\O''Brien\tools'` {
		t.Errorf("windowsQuoteArg = %s", got)
	}
}

func TestQuoteArg_FollowsThePlatform(t *testing.T) {
	dir := filepath.Join("opt", "my tools")
	if runtime.GOOS == "windows" {
		if got := quoteArg(dir); got != windowsQuoteArg(dir) {
			t.Errorf("quoteArg on windows = %s", got)
		}
		return
	}
	if got := quoteArg(dir); got != posixQuoteArg(dir) {
		t.Errorf("quoteArg on posix = %s", got)
	}
}

func TestUpdateAdvice_FollowsThePlatform(t *testing.T) {
	command, note := updateAdvice("/opt/bin")
	if runtime.GOOS == "windows" {
		if command != mustWindowsAdvice(t, "/opt/bin") {
			t.Error("updateAdvice did not use the windows rendering")
		}
		return
	}
	if !strings.Contains(command, InstallScriptURL) || strings.Contains(command, "brokit install") {
		t.Errorf("updateAdvice on posix did not use the installer: %s", command)
	}
	if len(note) == 0 {
		t.Error("updateAdvice returned no follow-up advice")
	}
}

func mustWindowsAdvice(t *testing.T, dir string) string {
	t.Helper()
	command, _ := windowsUpdateAdvice(dir)
	return command
}

func TestQuoteArg(t *testing.T) {
	tests := map[string]string{
		"/opt/bin":            "/opt/bin",
		"/opt/my tools":       `'/opt/my tools'`,
		"/opt/o'brien":        `'/opt/o'\''brien'`,
		"/opt/$HOME/bin":      `'/opt/$HOME/bin'`,
		"":                    "''",
		"/opt/bin;rm -rf /":   `'/opt/bin;rm -rf /'`,
		"/usr/local/my tools": `'/usr/local/my tools'`,
	}
	for in, want := range tests {
		if got := posixQuoteArg(in); got != want {
			t.Errorf("posixQuoteArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDocumentedCommandsMatchTheNotice pins the README to the commands the
// notice prints. They are maintained in separate files, so the agreement
// between them needs a test rather than a promise.
func TestDocumentedCommandsMatchTheNotice(t *testing.T) {
	readme := readReadme(t)

	for _, want := range []string{
		InstallScriptURL,
		"PROC_COMPOSE_INSTALL_DIR",
		"BROKIT_BIN",
		"brokit install proc-compose",
		UpdateCommand,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not document %q, which the update notice prints", want)
		}
	}
	if strings.Contains(readme, "PROC_COMPOSE_INSTALL_DIR=<dir> curl") {
		t.Error("README sets PROC_COMPOSE_INSTALL_DIR for curl, not for the installer")
	}
	// The positive form is the real parity check: with the assignment after the
	// pipe it is present, and flipping it back would make this fail.
	if runtime.GOOS != "windows" && !strings.Contains(readme, "| PROC_COMPOSE_INSTALL_DIR=") {
		t.Error("README does not show PROC_COMPOSE_INSTALL_DIR on the installer side of the pipe")
	}
}

// TestPosixUpdateAdvice_CommandRunsAndReachesTheInstaller executes the command
// the notice prints, against a local installer stub standing in for GitHub.
//
// Reading the rendered string cannot catch a shell-wiring mistake, and that is
// exactly the kind of bug this command had: the assignment has to reach the
// installer on the receiving end of the pipe. The control case runs the same
// command with the assignment before curl and asserts it delivers nothing, so
// this test cannot pass by accident.
func TestPosixUpdateAdvice_CommandRunsAndReachesTheInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the posix rendering is not runnable on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("executing the rendered command needs bash")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("executing the rendered command needs curl")
	}

	dir := t.TempDir()
	record := filepath.Join(dir, "received")
	// The stub records the directory it was told to install into and writes a
	// binary there, which is what the real installer does.
	stub := "#!/bin/sh\n" +
		"printf '%s' \"${PROC_COMPOSE_INSTALL_DIR:-}\" > '" + record + "'\n" +
		"if [ -z \"${PROC_COMPOSE_INSTALL_DIR:-}\" ]; then exit 0; fi\n" +
		"printf 'installed' > \"$PROC_COMPOSE_INSTALL_DIR/proc-compose\"\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, stub)
	}))
	defer srv.Close()

	command, _ := posixUpdateAdvice(dir)
	// Swap only the host, so the shell wiring under test is exactly what is
	// printed to the user.
	command = strings.Replace(command, InstallScriptURL, srv.URL+"/install.sh", 1)

	if out, err := exec.Command(bash, "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("the suggested command does not run: %v\n%s\ncommand: %s", err, out, command)
	}
	if got := readRecord(t, record); got != dir {
		t.Errorf("the installer received install dir %q, want %q", got, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "proc-compose")); err != nil {
		t.Errorf("the command did not update the binary in %s: %v", dir, err)
	}

	// Control: the assignment before curl sets it for curl, so the installer on
	// the other end of the pipe sees nothing and updates its own default.
	control := "PROC_COMPOSE_INSTALL_DIR='" + dir + "' curl -sSfL " + srv.URL + "/install.sh | bash"
	if out, err := exec.Command(bash, "-c", control).CombinedOutput(); err == nil {
		t.Logf("control output: %s", out)
	}
	if got := readRecord(t, record); got != "" {
		t.Errorf("control case delivered %q to the installer; the regression this guards is not covered", got)
	}
}

func readRecord(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stub installer never recorded a directory (%s): %v", path, err)
	}
	return string(data)
}

func readReadme(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	return string(data)
}

// noColour disables ANSI for one test. SetDisabled is the same switch --no-color
// flips; the ansi package reads NO_COLOR once at init, so setting the variable
// here would not take effect.
func noColour(t *testing.T) {
	t.Helper()
	ansi.SetDisabled(true)
	t.Cleanup(func() { ansi.SetDisabled(false) })
}

// ── cache reads ──────────────────────────────────────────────────────────────

func TestCached_FreshEntryIsUsed(t *testing.T) {
	c, path := testChecker(t)
	seedCache(t, path, "v1.3.0", time.Now().Add(-time.Hour))

	if got := c.Cached(); got != "v1.3.0" {
		t.Errorf("Cached() = %q, want v1.3.0", got)
	}
}

func TestCached_ExpiredEntryIsIgnored(t *testing.T) {
	c, path := testChecker(t)
	seedCache(t, path, "v1.3.0", time.Now().Add(-CacheTTL-time.Minute))

	if got := c.Cached(); got != "" {
		t.Errorf("Cached() = %q, want \"\" for an entry older than CacheTTL", got)
	}
}

// TestCached_FutureStampIgnored covers a clock-skewed or hand-edited cache: an
// entry stamped far in the future would otherwise pin the cache forever and
// suppress every future check.
func TestCached_FutureStampIgnored(t *testing.T) {
	c, path := testChecker(t)
	seedCache(t, path, "v1.3.0", time.Now().Add(365*24*time.Hour))

	if got := c.Cached(); got != "" {
		t.Errorf("Cached() = %q, want \"\" for an entry stamped beyond CacheTTL in the future", got)
	}
}

// TestCached_RecentClockSkewStillUsable keeps the future guard from throwing
// away an entry merely because the two clocks disagree by a few seconds.
func TestCached_RecentClockSkewStillUsable(t *testing.T) {
	c, path := testChecker(t)
	seedCache(t, path, "v1.3.0", time.Now().Add(5*time.Second))

	if got := c.Cached(); got != "v1.3.0" {
		t.Errorf("Cached() = %q, want v1.3.0 despite minor clock skew", got)
	}
}

func TestCached_MissingAndCorruptCacheAreSilent(t *testing.T) {
	c, path := testChecker(t)
	if got := c.Cached(); got != "" {
		t.Errorf("Cached() = %q, want \"\" when no cache exists", got)
	}

	for _, body := range []string{"", "not json", `{"latest":`, `{"latest":"v1.3.0","checked_at":"never"}`, `[]`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatalf("write corrupt cache: %v", err)
		}
		if got := c.Cached(); got != "" {
			t.Errorf("Cached() = %q for corrupt cache %q, want \"\"", got, body)
		}
	}
}

func TestCached_ConcurrentReadsAreSafe(t *testing.T) {
	c, path := testChecker(t)
	seedCache(t, path, "v1.3.0", time.Now())

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.Cached(); got != "v1.3.0" {
				t.Errorf("Cached() = %q under concurrency, want v1.3.0", got)
			}
		}()
	}
	wg.Wait()
}

// ── lookup ───────────────────────────────────────────────────────────────────

func TestRefresh_RecordsSuccessfulLookup(t *testing.T) {
	c, path := testChecker(t)
	srv, _ := testServer(t, http.StatusOK, releaseJSON("v1.3.0", false, false))
	c.endpoint = srv.URL

	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}
	if got := readCacheFile(t, path).Latest; got != "v1.3.0" {
		t.Errorf("cached latest = %q, want v1.3.0", got)
	}
	if got := c.Cached(); got != "v1.3.0" {
		t.Errorf("Cached() after Refresh = %q, want v1.3.0", got)
	}
}

func TestRefresh_IgnoresDraftPrereleaseAndUnusableTags(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"draft", releaseJSON("v1.3.0", true, false)},
		{"prerelease", releaseJSON("v1.3.0", false, true)},
		{"tag without v or patch", `{"tag_name":"nightly"}`},
		{"empty tag", `{"tag_name":""}`},
		{"tag with prerelease suffix", releaseJSON("v1.3.0-rc1", false, false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, path := testChecker(t)
			srv, _ := testServer(t, http.StatusOK, tt.body)
			c.endpoint = srv.URL

			if err := c.Refresh(); err == nil {
				t.Fatal("Refresh() = nil, want an error for an unusable release")
			}
			if got := readCacheFile(t, path).Latest; got != "" {
				t.Errorf("cached latest = %q, want \"\" so nothing is announced", got)
			}
		})
	}
}

func TestRefresh_UnsuccessfulResponsesAreRecordedAsAttempts(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		c, path := testChecker(t)
		srv, _ := testServer(t, status, releaseJSON("v1.3.0", false, false))
		c.endpoint = srv.URL

		if err := c.Refresh(); err == nil {
			t.Errorf("Refresh() = nil for status %d, want an error", status)
		}
		entry := readCacheFile(t, path)
		if entry.Latest != "" {
			t.Errorf("status %d: cached latest = %q, want \"\"", status, entry.Latest)
		}
		if time.Since(entry.CheckedAt) > time.Minute {
			t.Errorf("status %d: failed attempt was not stamped; offline machines would retry forever", status)
		}
	}
}

func TestRefresh_MalformedPayloadIsRecordedAsAttempt(t *testing.T) {
	for _, body := range []string{"", "not json", `{"tag_name":`, `{"tag_name":123}`} {
		c, path := testChecker(t)
		srv, _ := testServer(t, http.StatusOK, body)
		c.endpoint = srv.URL

		if err := c.Refresh(); err == nil {
			t.Errorf("Refresh() = nil for body %q, want an error", body)
		}
		if got := readCacheFile(t, path).Latest; got != "" {
			t.Errorf("body %q: cached latest = %q, want \"\"", body, got)
		}
	}
}

// TestRefresh_UnreachableEndpointTimesOut proves the lookup is bounded: an API
// that accepts the connection and never answers must not hold the check open.
func TestRefresh_UnreachableEndpointTimesOut(t *testing.T) {
	c, path := testChecker(t)
	// The handler never answers until the test releases it, so the client's
	// timeout is the only thing that can end the request. Cleanups run LIFO:
	// the handler is released before the server waits for it.
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hang) })
	c.endpoint = srv.URL
	c.client = &http.Client{Timeout: 100 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- c.Refresh() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Refresh() = nil, want a timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh() did not honour its HTTP timeout")
	}
	// The attempt is still recorded, so the next invocation does not retry
	// immediately against the same dead endpoint.
	if got := readCacheFile(t, path).Latest; got != "" {
		t.Errorf("cached latest = %q after a timeout, want \"\"", got)
	}
}

// TestRefresh_OversizedResponseIsBounded proves the body read is capped: a
// server that streams far past maxBodyBytes must not be drained in full.
func TestRefresh_OversizedResponseIsBounded(t *testing.T) {
	c, _ := testChecker(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 64*1024)
		for i := 0; i < 64; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c.endpoint = srv.URL

	done := make(chan error, 1)
	go func() { done <- c.Refresh() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Refresh() = nil for an oversized payload, want an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Refresh() read an unbounded response body")
	}
}

func TestRefresh_UnwritableCacheIsSilent(t *testing.T) {
	c, _ := testChecker(t)
	srv, _ := testServer(t, http.StatusOK, releaseJSON("v1.3.0", false, false))
	c.endpoint = srv.URL
	// A cache path whose parent does not exist and cannot be created.
	c.cachePath = filepath.Join(c.cachePath, "nested", CacheFileName)

	// A failed cache write must not be reported as a usable version.
	if got := c.Cached(); got != "" {
		t.Errorf("Cached() = %q with no cache directory, want \"\"", got)
	}
	if latest := c.Notice("v1.0.0"); latest != "" {
		t.Errorf("Notice() = %q with an unwritable cache, want \"\"", latest)
	}
}

func TestRefresh_ConcurrentWritesLeaveACompleteFile(t *testing.T) {
	c, path := testChecker(t)
	srv, _ := testServer(t, http.StatusOK, releaseJSON("v1.3.0", false, false))
	c.endpoint = srv.URL

	// Writers and readers race on the same entry, as separate CLI
	// invocations in different projects would.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Refresh(); err != nil {
				t.Errorf("Refresh() = %v", err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Cached()
		}()
	}
	wg.Wait()

	if got := readCacheFile(t, path).Latest; got != "v1.3.0" {
		t.Errorf("cached latest = %q after concurrent writes, want v1.3.0", got)
	}
	// No staging files may be left behind by a successful rename.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != CacheFileName {
			t.Errorf("cache dir contains leftover staging file %q", e.Name())
		}
	}
}

// ── Notice ───────────────────────────────────────────────────────────────────

func TestNotice_AnnouncesFromFreshCacheWithoutNetwork(t *testing.T) {
	c, path := testChecker(t)
	srv, hits := testServer(t, http.StatusOK, releaseJSON("v1.3.0", false, false))
	c.endpoint = srv.URL
	seedCache(t, path, "v1.3.0", time.Now())

	if got := c.Notice("v1.2.0"); got != "v1.3.0" {
		t.Errorf("Notice() = %q, want v1.3.0", got)
	}
	if n := hits(); n != 0 {
		t.Errorf("Notice() made %d HTTP requests despite a fresh cache; want 0", n)
	}
}

func TestNotice_SilentWhenNotNewerOrNotComparable(t *testing.T) {
	tests := []struct{ installed, latest string }{
		{"v1.3.0", "v1.3.0"},
		{"v1.4.0", "v1.3.0"},
		{"dev", "v1.3.0"},
		{"v1.2.0-3-gabc", "v1.3.0"},
	}
	for _, tt := range tests {
		c, path := testChecker(t)
		srv, hits := testServer(t, http.StatusOK, releaseJSON(tt.latest, false, false))
		c.endpoint = srv.URL
		seedCache(t, path, tt.latest, time.Now())

		if got := c.Notice(tt.installed); got != "" {
			t.Errorf("Notice(%q) with cached %q = %q, want \"\"", tt.installed, tt.latest, got)
		}
		if n := hits(); n != 0 {
			t.Errorf("Notice(%q) made %d HTTP requests; a fresh cache needs none", tt.installed, n)
		}
	}
}

// TestNotice_StaleCacheRefreshesInBackgroundWithoutBlocking is the property the
// CLI depends on: a command must not wait on the network, and the refreshed
// result must become visible to a later invocation.
func TestNotice_StaleCacheRefreshesInBackgroundWithoutBlocking(t *testing.T) {
	c, _ := testChecker(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseEndpoint := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(releaseJSON("v1.3.0", false, false)))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(releaseEndpoint)
	c.endpoint = srv.URL

	// No cache at all: Notice must return before the endpoint ever replies.
	started := time.Now()
	if got := c.Notice("v1.2.0"); got != "" {
		t.Fatalf("Notice() = %q with no cache, want \"\"", got)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Notice() blocked for %s waiting on the network", elapsed)
	}

	// Once the endpoint answers, a later invocation sees the result.
	releaseEndpoint()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := c.Notice("v1.2.0"); got == "v1.3.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never recorded the release")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNotice_FailedAttemptBacksOffWithoutReRequesting is the offline and
// rate-limited case: once a failure is on record, subsequent invocations must
// stay off the network until the record expires.
func TestNotice_FailedAttemptBacksOffWithoutReRequesting(t *testing.T) {
	c, path := testChecker(t)
	srv, hits := testServer(t, http.StatusForbidden, `{}`)
	c.endpoint = srv.URL
	if err := c.Refresh(); err == nil {
		t.Fatal("expected a failed lookup")
	}

	for i := 0; i < 3; i++ {
		// Each invocation gets a new Checker, just like the CLI.
		next := New(path)
		next.endpoint = srv.URL
		if got := next.Notice("v1.2.0"); got != "" {
			t.Fatalf("Notice() = %q after a recorded failure, want \"\"", got)
		}
		// Check scheduling synchronously: counting HTTP hits immediately after
		// Notice would miss a goroutine that has not started running yet.
		unscheduled := false
		next.refreshOnce.Do(func() { unscheduled = true })
		if !unscheduled {
			t.Fatal("a fresh failed attempt scheduled another lookup")
		}
	}
	if n := hits(); n != 1 {
		t.Errorf("made %d HTTP requests including the initial failure, want 1", n)
	}
}

func TestNotice_UnusableVersionOrCacheDoesNotScheduleLookup(t *testing.T) {
	for _, installed := range []string{"dev", "v1.2.0-rc1", "v1.2.0-dirty", "abc123", "v1.2.0"} {
		t.Run(installed, func(t *testing.T) {
			c, _ := testChecker(t)
			if installed == "v1.2.0" {
				c.cachePath = "" // unavailable user cache
			}
			if got := c.Notice(installed); got != "" {
				t.Fatalf("unexpected notice: %q", got)
			}
			unscheduled := false
			c.refreshOnce.Do(func() { unscheduled = true })
			if !unscheduled {
				t.Fatal("an unusable version or cache scheduled a lookup")
			}
		})
	}
}

// TestNotice_SchedulesAtMostOneRefreshPerChecker guards against a command that
// consults the check more than once re-running the lookup each time.
func TestNotice_SchedulesAtMostOneRefreshPerChecker(t *testing.T) {
	c, _ := testChecker(t)
	srv, hits := testServer(t, http.StatusOK, releaseJSON("v1.3.0", false, false))
	c.endpoint = srv.URL

	for i := 0; i < 5; i++ {
		c.Notice("v1.2.0")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if hits() >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := hits(); n != 1 {
		t.Errorf("made %d HTTP requests for 5 notices, want 1", n)
	}
}

// ── defaults ─────────────────────────────────────────────────────────────────

func TestDefault_UsesSharedUserCacheDirectory(t *testing.T) {
	// Shared across projects, never written into a project directory.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")

	got := Default().cachePath
	if !strings.HasSuffix(got, filepath.Join("proc-compose", CacheFileName)) {
		t.Errorf("Default() cache path = %q, want a path under the shared proc-compose cache dir", got)
	}
}
