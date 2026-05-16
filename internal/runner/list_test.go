package runner

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
)

func TestListProcesses_TaskModeDisplayed(t *testing.T) {
	r := &Runner{
		Config: &config.Config{
			Processes: map[string]config.Process{
				"my-task": {Cmd: "echo hello", Mode: "task"},
				"my-svc":  {Cmd: "echo hello", Restart: "always"},
			},
		},
		NoColor: true,
	}

	pr, pw, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = pw
	r.ListProcesses()
	pw.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	buf.ReadFrom(pr)
	output := buf.String()

	if !strings.Contains(output, "(task)") {
		t.Error("expected (task) for task-mode process in list output")
	}
	if !strings.Contains(output, "my-task") {
		t.Error("expected my-task in list output")
	}
	if !strings.Contains(output, "my-svc") {
		t.Error("expected my-svc in list output")
	}
	if !strings.Contains(output, "(always)") {
		t.Error("service process should show (always) in list output")
	}
}
