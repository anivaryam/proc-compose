// Package update implements a best-effort, notification-only check for newer
// proc-compose releases.
//
// It answers exactly one question — "is the installed binary older than the
// latest published release?" — and prints the supported upgrade command. It
// never downloads, installs, or replaces anything.
//
// The check never blocks the command that triggered it. A cached result is
// reused for CacheTTL; when that cache is missing or stale the lookup runs in
// the background so the notice surfaces on the next invocation. Every failure
// mode — no network, rate limiting, an unparseable tag, an unwritable cache —
// is silent, because an update hint is never worth changing what a command
// prints or the status it exits with.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/paths"
)

const (
	// ReleaseURL describes the latest published release of this repository.
	ReleaseURL = "https://api.github.com/repos/anivaryam/proc-compose/releases/latest"

	// CacheTTL is how long a recorded lookup stays usable. Failed attempts are
	// recorded for the same window, so an offline or rate-limited machine does
	// not re-request on every single invocation.
	CacheTTL = 24 * time.Hour

	// CacheFileName is the file inside paths.Cache() holding the last lookup.
	// Shared by every project on the machine, never written into a project
	// directory.
	CacheFileName = "update-check.json"

	// UpdateCommand is the supported way to update a copy brokit manages.
	UpdateCommand = "brokit update proc-compose"

	// InstallScriptURL is proc-compose's own single-tool installer. It honours
	// PROC_COMPOSE_INSTALL_DIR, which is what makes it able to update a specific
	// copy rather than adding another one somewhere else.
	InstallScriptURL = "https://raw.githubusercontent.com/anivaryam/proc-compose/main/install.sh"

	// maxBodyBytes bounds how much of the GitHub response is read. The release
	// document is a few KB; anything past this is not ours.
	maxBodyBytes = 1 << 20

	// httpTimeout bounds the background lookup. It is short because nothing
	// waits on it: a slow or unreachable API just means no refresh.
	httpTimeout = 3 * time.Second
)

// Message renders the notice for a newer release, using the upgrade commands
// that fit the copy that is actually running.
func Message(installed, latest string) string {
	return MessageFor(installed, latest, Detect())
}

// semver is a parsed MAJOR.MINOR.PATCH triple.
type semver struct{ major, minor, patch int }

// parseVersion parses a stable release version: an optional leading "v"
// followed by exactly three dot-separated decimal components.
//
// Everything else is rejected — "dev", a bare commit hash, and anything with a
// suffix after the patch number such as v1.2.0-rc1, v1.2.0-3-gabc123, or
// v1.2.0-dirty. Those describe a development or prerelease build, and
// comparing one against a published release would produce advice the user
// cannot act on.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var out [3]int
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return semver{}, false
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return semver{}, false
		}
		out[i] = n
	}
	return semver{major: out[0], minor: out[1], patch: out[2]}, true
}

// Newer reports whether latest is strictly newer than installed. An
// unparseable version on either side is never newer, so a development build is
// never nagged and a malformed tag never hides a real upgrade.
func Newer(installed, latest string) bool {
	cur, ok := parseVersion(installed)
	if !ok {
		return false
	}
	next, ok := parseVersion(latest)
	if !ok {
		return false
	}
	return less(cur, next)
}

// less orders two parsed versions. Components are compared most significant
// first so 1.10.0 is newer than 1.9.0 — a string compare would get that wrong.
func less(a, b semver) bool {
	switch {
	case a.major != b.major:
		return a.major < b.major
	case a.minor != b.minor:
		return a.minor < b.minor
	default:
		return a.patch < b.patch
	}
}

// cacheEntry is the on-disk record of the last lookup attempt. CheckedAt
// stamps the attempt, not the success: a machine that cannot reach the API
// must still record when it last tried so the next invocation does not retry
// immediately.
type cacheEntry struct {
	// Latest is the newest release tag observed, or "" when the last attempt
	// failed. A fresh entry with an empty Latest keeps the request backoff
	// while announcing nothing.
	Latest string `json:"latest"`
	// CheckedAt is when the lookup was attempted, successfully or not.
	CheckedAt time.Time `json:"checked_at"`
}

// Checker reads and refreshes the cached latest-release lookup. Build one with
// New or Default.
type Checker struct {
	cachePath   string
	endpoint    string
	client      *http.Client
	now         func() time.Time
	refreshOnce sync.Once
}

// New returns a Checker backed by cachePath.
func New(cachePath string) *Checker {
	return &Checker{
		cachePath: cachePath,
		endpoint:  ReleaseURL,
		client:    &http.Client{Timeout: httpTimeout},
		now:       time.Now,
	}
}

// Default returns a Checker backed by the shared per-user cache directory.
func Default() *Checker {
	return New(paths.CacheFile(CacheFileName))
}

// Cached returns the newest release tag recorded by a lookup still within
// CacheTTL, or "" when the cache is missing, corrupt, or expired. It performs
// no I/O beyond reading one small file and never contacts the network.
func (c *Checker) Cached() string {
	entry, fresh := c.cachedEntry()
	if !fresh {
		return ""
	}
	return entry.Latest
}

// cachedEntry keeps freshness distinct from an empty (failed) lookup result.
func (c *Checker) cachedEntry() (cacheEntry, bool) {
	entry, err := c.read()
	if err != nil {
		return entry, false
	}
	now := c.now()
	if now.Sub(entry.CheckedAt) >= CacheTTL {
		return entry, false
	}
	// A stamp from the future beyond the TTL is a hand-edited or clock-skewed
	// file; treating it as fresh would pin the cache forever.
	if entry.CheckedAt.After(now.Add(CacheTTL)) {
		return entry, false
	}
	return entry, true
}

// Notice returns the release tag when the cache proves a newer release exists,
// and otherwise arranges a background refresh for a later invocation. It never
// waits on the network and never returns an error.
func (c *Checker) Notice(installed string) string {
	if _, ok := parseVersion(installed); !ok || c.cachePath == "" {
		return ""
	}
	entry, fresh := c.cachedEntry()
	if fresh {
		if Newer(installed, entry.Latest) {
			return entry.Latest
		}
		return ""
	}
	c.refreshAsync()
	return ""
}

// Refresh performs the lookup and records the outcome. It blocks on the
// network, so callers that must not stall a command run it in the background.
//
// It is safe to call concurrently and across processes: entries are written
// atomically, so a concurrent reader sees either the previous entry or the new
// one, never a half-written file.
func (c *Checker) Refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	return c.refresh(ctx)
}

// refreshAsync schedules one background lookup for this Checker. The goroutine
// may be cut short when the process exits before the HTTP timeout elapses; that
// is harmless, because a missing entry only means the next invocation tries
// again.
func (c *Checker) refreshAsync() {
	c.refreshOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
			defer cancel()
			_ = c.refresh(ctx)
		}()
	})
}

func (c *Checker) refresh(ctx context.Context) error {
	tag, err := c.fetchLatest(ctx)
	entry := cacheEntry{CheckedAt: c.now()}
	if err == nil {
		entry.Latest = tag
	}
	if writeErr := c.write(entry); writeErr != nil && err == nil {
		err = writeErr
	}
	return err
}

// releasePayload is the part of the GitHub release document this check reads.
// The document carries much more (assets, release notes); decoding only these
// fields keeps the response bounded and the parse immune to unrelated additions.
type releasePayload struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func (c *Checker) fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "proc-compose")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update check: unexpected status %s", resp.Status)
	}

	var payload releasePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&payload); err != nil {
		return "", fmt.Errorf("update check: cannot read response: %w", err)
	}
	if payload.Draft {
		return "", fmt.Errorf("update check: latest release is a draft")
	}
	if payload.Prerelease {
		return "", fmt.Errorf("update check: latest release is a prerelease")
	}
	if _, ok := parseVersion(payload.TagName); !ok {
		return "", fmt.Errorf("update check: unusable release tag %q", payload.TagName)
	}
	return payload.TagName, nil
}

func (c *Checker) read() (cacheEntry, error) {
	var entry cacheEntry
	data, err := os.ReadFile(c.cachePath)
	if err != nil {
		return entry, err
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, fmt.Errorf("update check: corrupt cache %s: %w", c.cachePath, err)
	}
	return entry, nil
}

// write records the attempt atomically. Each writer stages its own file in the
// cache directory and renames it into place, so two invocations racing on the
// same entry both produce a complete file and the last rename wins.
func (c *Checker) write(entry cacheEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.cachePath), filepath.Base(c.cachePath)+".*")
	if err != nil {
		return fmt.Errorf("update check: cannot write cache %s: %w", c.cachePath, err)
	}
	name := tmp.Name()
	// No-op once the rename below succeeds.
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("update check: cannot write cache %s: %w", c.cachePath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update check: cannot write cache %s: %w", c.cachePath, err)
	}
	for attempt := 0; ; attempt++ {
		err := os.Rename(name, c.cachePath)
		if err == nil {
			return nil
		}
		// Windows briefly denies replacement while another reader or writer
		// holds the destination open. Keep atomic replacement and retry only
		// this permission error, for at most 200ms, in the background writer.
		if runtime.GOOS != "windows" || !os.IsPermission(err) || attempt == 20 {
			return fmt.Errorf("update check: cannot write cache %s: %w", c.cachePath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
