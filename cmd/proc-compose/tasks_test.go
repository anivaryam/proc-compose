package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anivaryam/proc-compose/internal/jobs"
)

func TestTasksCLI_DiscoverForegroundAndBackground(t *testing.T) {
	bin := buildTestBinary(t)
	dir := t.TempDir()
	t.Setenv("PROC_COMPOSE_JOB_DIR", t.TempDir())
	argv, _ := json.Marshal([]string{bin, "--version"})
	writeCLIFile(t, filepath.Join(dir, "proc-compose.yml"), "tasks:\n  version:\n    description: Existing command\n    argv: "+string(argv)+"\n    timeout: 5\n")
	stdout, stderr, code := runVerifyCLI(t, bin, dir, "tasks", "list", "--json")
	catalog := decodeOne(t, stdout)
	if code != 0 || catalog["kind"] != "tasks.catalog" || len(catalog["tasks"].([]any)) != 1 {
		t.Fatalf("catalog: %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "tasks", "run", "version", "--json")
	result := decodeOne(t, stdout)
	if code != 0 || result["task_name"] != "version" || result["standalone"] != true || result["config_sha256"] == nil {
		t.Fatalf("foreground: %d %s %s", code, stdout, stderr)
	}
	stdout, stderr, code = runVerifyCLI(t, bin, dir, "tasks", "run", "version", "--background", "--json")
	if code != 0 {
		t.Fatalf("background: %d %s %s", code, stdout, stderr)
	}
	job := decodeJob(t, stdout)
	store, err := jobs.Open()
	if err != nil {
		t.Fatal(err)
	}
	cleanupJob(t, store, job.ID)
	stdout, stderr, code = runVerifyCLI(t, bin, t.TempDir(), "jobs", "wait", job.ID, "--json", "--timeout", "10s")
	job = decodeJob(t, stdout)
	if code != 0 || !job.Terminal || job.TaskName != "version" || job.Result.TaskName != "version" {
		t.Fatalf("background task provenance: %d %s %s", code, stdout, stderr)
	}
	for _, args := range [][]string{{"tasks", "run", "missing", "--json"}, {"tasks", "run", "version", "--json", "--", "extra"}} {
		stdout, stderr, code = runVerifyCLI(t, bin, dir, args...)
		if result := decodeOne(t, stdout); code != 2 || result["status"] != "failed" {
			t.Fatalf("refusal: %d %s %s", code, stdout, stderr)
		}
	}
}
