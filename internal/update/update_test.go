package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestMessage_NamesBothVersionsAndUpdateCommand(t *testing.T) {
	// SetDisabled is the same switch --no-color flips; the ansi package reads
	// NO_COLOR once at init, so setting it here would not take effect.
	ansi.SetDisabled(true)
	t.Cleanup(func() { ansi.SetDisabled(false) })

	got := Message("v1.2.0", "v1.3.0")

	for _, want := range []string{"v1.2.0", "v1.3.0", UpdateCommand, "brokit update proc-compose"} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\033") {
		t.Errorf("message contains ANSI escapes with colour disabled:\n%q", got)
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
