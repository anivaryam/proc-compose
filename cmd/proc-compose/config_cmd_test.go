package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
)

func runConfigCommand(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := newConfigCommand(&configPath)
	command.SilenceErrors = true
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestConfigCommand_SchemaPrintsVersionedSchema(t *testing.T) {
	out, err := runConfigCommand(t, "unused.yml", "schema")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(config.Schema()) || !strings.Contains(out, config.SchemaID) {
		t.Fatalf("unexpected schema output:\n%s", out)
	}
}

func TestConfigCommand_CheckJSONAndHuman(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	writeCLIFile(t, path, "processes:\n  api:\n    cmd: x\n    depends_on: [db]\n")

	out, err := runConfigCommand(t, path, "check", "--json")
	if err == nil {
		t.Fatal("invalid config must exit non-zero")
	}
	var result struct {
		SchemaVersion int                 `json:"schema_version"`
		Kind          string              `json:"kind"`
		Valid         bool                `json:"valid"`
		Diagnostics   []config.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if result.SchemaVersion != 1 || result.Kind != "config.diagnostics" || result.Valid || len(result.Diagnostics) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if d := result.Diagnostics[0]; d.Code != config.CodeUnknownDependency || d.Path != "processes.api.depends_on[0]" || d.Line != 4 {
		t.Fatalf("unexpected diagnostic: %+v", d)
	}

	out, err = runConfigCommand(t, path, "check")
	if err == nil || !strings.Contains(out, path+":4:18: unknown_dependency processes.api.depends_on[0]: ") || !strings.Contains(out, "hint: ") {
		t.Fatalf("unexpected human output (err=%v):\n%s", err, out)
	}

	writeCLIFile(t, path, "processes:\n  api:\n    cmd: x\n")
	if out, err := runConfigCommand(t, path, "check"); err != nil || !strings.HasPrefix(out, "ok: ") {
		t.Fatalf("valid config failed check (err=%v):\n%s", err, out)
	}
}

func TestConfigCommand_PlanIsNotLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	writeCLIFile(t, path, "processes:\n  db:\n    cmd: db\n    ports:\n      pg: 0\n  api:\n    cmd: api\n    depends_on: [db]\n    env:\n      DB: ${urls.db.pg}\n  web:\n    cmd: web\n")

	out, err := runConfigCommand(t, path, "plan", "api", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var plan config.StartupPlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if plan.Live || plan.Kind != "config.plan" || plan.SchemaVersion != 1 || len(plan.Processes) != 2 || plan.Processes[1].Name != "api" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if !strings.Contains(out, `"live": false`) {
		t.Fatalf("plan JSON must state live false:\n%s", out)
	}

	out, err = runConfigCommand(t, path, "plan")
	if err != nil || !strings.Contains(out, "plan (not live)") || !strings.Contains(out, "env.DB←urls.db.pg") {
		t.Fatalf("unexpected human plan (err=%v):\n%s", err, out)
	}
	if _, err := runConfigCommand(t, path, "plan", "missing"); err == nil {
		t.Fatal("unknown process accepted")
	}
}
