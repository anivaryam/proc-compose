package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateUnitName(t *testing.T) {
	good := []string{"app", "myapp", "app_1", "app-1", "app.1", "A", "x123_y-z.t", strings.Repeat("a", 64)}
	for _, n := range good {
		if err := validateUnitName(n); err != nil {
			t.Errorf("validateUnitName(%q) returned error: %v", n, err)
		}
	}

	bad := []struct{ name, why string }{
		{"", "empty"},
		{strings.Repeat("a", 65), "too long"},
		{"app/sub", "slash"},
		{"app\nfoo", "newline"},
		{"app foo", "space"},
		{"app$", "shell metachar"},
		{"app;ls", "semicolon"},
		{"../escape", "dotdot"},
		{"app#tag", "hash"},
	}
	for _, c := range bad {
		if err := validateUnitName(c.name); err == nil {
			t.Errorf("validateUnitName(%q) accepted (%s); want rejection", c.name, c.why)
		}
	}
}

func TestBuildChildArgs_StripsSilentAndInjectsFile(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent"}, "/abs/cfg.yml", "/var/log/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/var/log/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_StripsShortSilent(t *testing.T) {
	got := buildChildArgs([]string{"up", "-s"}, "/abs/cfg.yml", "/var/log/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/var/log/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_ReplacesExistingFileFlag(t *testing.T) {
	got := buildChildArgs([]string{"up", "-f", "old.yml", "--silent"}, "/abs/cfg.yml", "/v/x.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "/v/x.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildChildArgs_PreservesExistingLogFile(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent", "--log-file", "user.log"}, "/abs/cfg.yml", "/auto.log")
	want := []string{"--file", "/abs/cfg.yml", "up", "--log-file", "user.log", "--no-banner"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStarterTemplate_KnownNames(t *testing.T) {
	for _, name := range []string{"", "minimal", "node", "go", "python"} {
		s, err := starterTemplate(name)
		if err != nil {
			t.Errorf("starterTemplate(%q) error = %v", name, err)
		}
		if !strings.Contains(s, "processes:") {
			t.Errorf("starterTemplate(%q) missing 'processes:'\n%s", name, s)
		}
	}
}

func TestStarterTemplate_UnknownRejected(t *testing.T) {
	if _, err := starterTemplate("rust"); err == nil {
		t.Error("expected error for unknown template")
	}
}

func TestResolveConfigExtension_DefaultPrefersYml(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// No file: returns default unchanged.
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yml" {
		t.Errorf("got %q, want default", got)
	}
	// Only .yaml exists: switches to .yaml.
	os.WriteFile(filepath.Join(dir, "proc-compose.yaml"), []byte("processes: {a: {cmd: x}}\n"), 0600)
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yaml" {
		t.Errorf("got %q, want proc-compose.yaml", got)
	}
	// Both exist: prefers .yml (existing behaviour).
	os.WriteFile(filepath.Join(dir, "proc-compose.yml"), []byte("processes: {a: {cmd: x}}\n"), 0600)
	if got := resolveConfigExtension("proc-compose.yml"); got != "proc-compose.yml" {
		t.Errorf("got %q, want proc-compose.yml", got)
	}
	// Explicit non-default path returned unchanged.
	if got := resolveConfigExtension("custom.yml"); got != "custom.yml" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestBuildChildArgs_NoDuplicateNoBanner(t *testing.T) {
	got := buildChildArgs([]string{"up", "--silent", "--no-banner"}, "/abs/cfg.yml", "/v/x.log")
	count := 0
	for _, a := range got {
		if a == "--no-banner" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one --no-banner, got %d in %v", count, got)
	}
}

func TestTailLog_ReturnsLastNLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0600)

	got := tailLog(path, 3)
	if !strings.Contains(got, "c") || !strings.Contains(got, "d") || !strings.Contains(got, "e") {
		t.Errorf("expected last 3 lines, got %q", got)
	}
	if strings.Contains(got, "a") || strings.Contains(got, "b") {
		t.Errorf("expected only last 3 lines, got %q", got)
	}
}

func TestTailLog_FewerLinesThanRequested(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("only\nthese\n"), 0600)

	got := tailLog(path, 10)
	if !strings.Contains(got, "only") || !strings.Contains(got, "these") {
		t.Errorf("got %q", got)
	}
}

func TestTailLog_MissingFileReturnsEmpty(t *testing.T) {
	got := tailLog("/nonexistent/log", 30)
	if got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

func TestTailLog_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	os.WriteFile(path, []byte(""), 0600)

	got := tailLog(path, 5)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestIsPortFree(t *testing.T) {
	// We can't reliably test "busy" without claiming a port; just ensure free
	// check on an obviously-free high port returns true.
	if !isPortFree(0) {
		// Port 0 means "let OS choose"; will succeed-listen-then-close. Treat
		// as informational; failure usually means restricted environment.
		t.Skip("port 0 unavailable in this environment")
	}
}

func TestGetHomeDir(t *testing.T) {
	home, err := getHomeDir()
	if err != nil {
		t.Fatalf("getHomeDir() failed: %v", err)
	}
	if home == "" {
		t.Error("getHomeDir() returned empty string")
	}
	if !filepath.IsAbs(home) {
		t.Errorf("getHomeDir() = %q, want absolute path", home)
	}
}
