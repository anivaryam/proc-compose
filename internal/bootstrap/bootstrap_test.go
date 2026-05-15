package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModeFromOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want Mode
	}{
		{name: "dry run", opts: Options{}, want: ModeDryRun},
		{name: "write", opts: Options{Write: true}, want: ModeWrite},
		{name: "verify", opts: Options{Verify: true}, want: ModeVerify},
		{name: "write verify", opts: Options{Write: true, Verify: true}, want: ModeWriteVerify},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modeFromOptions(tt.opts); got != tt.want {
				t.Fatalf("modeFromOptions() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewReportInitializesSlices(t *testing.T) {
	report := newReport(Options{Write: true, Verify: true}, "/tmp/proc-compose.yml")
	if report.Mode != ModeWriteVerify {
		t.Fatalf("mode = %q, want %q", report.Mode, ModeWriteVerify)
	}
	if report.ConfigPath != "/tmp/proc-compose.yml" {
		t.Fatalf("config path = %q", report.ConfigPath)
	}
	if report.Observations == nil || report.Suggestions == nil || report.AppliedChanges == nil {
		t.Fatalf("report slices must be initialized: %+v", report)
	}
}

func TestRunDryRunUsesDoctorSuggestedConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite --host 127.0.0.1"},"dependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Mode != ModeDryRun {
		t.Fatalf("mode = %q", report.Mode)
	}
	if report.Doctor == nil || !strings.Contains(report.Doctor.SuggestedYAML, "npm run dev") {
		t.Fatalf("missing doctor suggested YAML: %+v", report.Doctor)
	}
	if _, err := os.Stat(filepath.Join(dir, "proc-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("dry run must not write config, stat err=%v", err)
	}
}

func TestRunWriteRefusesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), "processes: {}\n")

	report, err := Run(Options{Root: dir, Write: true})
	if err == nil {
		t.Fatal("expected write refusal error")
	}
	if report == nil || report.Mode != ModeWrite {
		t.Fatalf("report mode = %+v", report)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %q, want already exists", err.Error())
	}
}

func TestRunWriteCreatesConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"},"dependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir, Write: true})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "proc-compose.yml"))
	if err != nil {
		t.Fatalf("expected written config: %v", err)
	}
	if !strings.Contains(string(data), "npm run dev") {
		t.Fatalf("written config missing command:\n%s", string(data))
	}
	if len(report.AppliedChanges) != 1 || report.AppliedChanges[0].Path != filepath.Join(dir, "proc-compose.yml") {
		t.Fatalf("applied changes = %+v", report.AppliedChanges)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWriteJSON(t *testing.T) {
	report := newReport(Options{Verify: true}, "/tmp/proc-compose.yml")
	report.Observations = append(report.Observations, Observation{Kind: "process_exit", Message: "api exited"})

	var buf strings.Builder
	if err := WriteJSON(&buf, report); err != nil {
		t.Fatalf("WriteJSON returned error: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, `"mode": "verify"`) || !strings.Contains(text, `"process_exit"`) {
		t.Fatalf("unexpected JSON:\n%s", text)
	}
}

func TestWriteText(t *testing.T) {
	report := newReport(Options{Write: true}, "/tmp/proc-compose.yml")
	report.Suggestions = append(report.Suggestions, Suggestion{Code: "manual_fix", Message: "set DATABASE_URL"})
	report.AppliedChanges = append(report.AppliedChanges, AppliedChange{Path: "/tmp/proc-compose.yml", Message: "wrote generated proc-compose config"})

	var buf strings.Builder
	if err := WriteText(&buf, report); err != nil {
		t.Fatalf("WriteText returned error: %v", err)
	}
	text := buf.String()
	if !strings.Contains(text, "proc-compose bootstrap") || !strings.Contains(text, "set DATABASE_URL") || !strings.Contains(text, "wrote generated") {
		t.Fatalf("unexpected text:\n%s", text)
	}
}

type fakeVerifier struct {
	result *Verification
	err    error
}

func (f fakeVerifier) Verify(configPath string) (*Verification, error) {
	return f.result, f.err
}

func TestRunVerifyUsesTemporaryConfigWhenNotWriting(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"},"dependencies":{"vite":"latest"}}`)

	report, err := Run(Options{
		Root:     dir,
		Verify:   true,
		Verifier: fakeVerifier{result: &Verification{Started: true, Ready: true, Stopped: true}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Verification == nil || !report.Verification.Ready {
		t.Fatalf("verification = %+v", report.Verification)
	}
	if _, err := os.Stat(filepath.Join(dir, "proc-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("verify without write must not create final config")
	}
}

func TestRunWriteVerifyVerifiesWrittenConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"},"dependencies":{"vite":"latest"}}`)

	report, err := Run(Options{
		Root:     dir,
		Write:    true,
		Verify:   true,
		Verifier: fakeVerifier{result: &Verification{Started: true, Ready: true, Stopped: true}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Verification == nil || report.Verification.ConfigPath != filepath.Join(dir, "proc-compose.yml") {
		t.Fatalf("verification path = %+v", report.Verification)
	}
}
