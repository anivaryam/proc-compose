//go:build !windows

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRun_CheckDescendantsAreCleanedUp(t *testing.T) {
	f := newFixture(t, serviceConfig)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	// The check leader exits at once and leaves a background child behind.
	result := Run(context.Background(), f.options(t, "", "/bin/sh", "-c", `sleep 60 & echo $! > "$1"`, "sh", childPID))
	if result.Check.Status != StatusPassed || result.Cleanup.Status != StatusPassed || result.Status != StatusPassed {
		t.Fatalf("check %s cleanup %+v errors %v", result.Check.Status, result.Cleanup, result.Errors)
	}
	data, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	assertGone(t, pid)
}

func TestRun_EscapedCheckDescendantIsUnverified(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid not available")
	}
	f := newFixture(t, serviceConfig)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	opts := f.options(t, "", "/bin/sh", "-c", `setsid sh -c 'echo $$ > "$1"; exec sleep 30' sh "$1" & sleep 0.5`, "sh", childPID)
	result := Run(context.Background(), opts)
	data, _ := os.ReadFile(childPID)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		t.Cleanup(func() {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		})
	}
	if result.Status == StatusPassed || result.Cleanup.Status != StatusUnverified || result.Cleanup.TerminationVerified == nil || *result.Cleanup.TerminationVerified {
		t.Fatalf("escaped descendant reported as clean: status %s cleanup %+v", result.Status, result.Cleanup)
	}
}
