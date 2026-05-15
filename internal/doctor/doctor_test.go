package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
)

func TestScanDetectsNodePackage(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "web"), 0755); err != nil {
		t.Fatalf("failed to create web dir: %v", err)
	}
	pkgJSON := `{"scripts":{"dev":"vite --host 0.0.0.0"},"dependencies":{"vite":"latest"}}`
	if err := os.WriteFile(filepath.Join(dir, "web", "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatalf("failed to write package.json: %v", err)
	}

	envExample := "PORT=5173\n"
	if err := os.WriteFile(filepath.Join(dir, "web", ".env.example"), []byte(envExample), 0644); err != nil {
		t.Fatalf("failed to write .env.example: %v", err)
	}

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(report.Services))
	}

	svc := report.Services[0]
	if svc.Name != "web" {
		t.Errorf("service name = %q, want %q", svc.Name, "web")
	}
	if svc.Dir != "./web" {
		t.Errorf("service dir = %q, want %q", svc.Dir, "./web")
	}
	if svc.Command != "npm run dev" {
		t.Errorf("service command = %q, want %q", svc.Command, "npm run dev")
	}
	if svc.Port != 5173 {
		t.Errorf("service port = %d, want %d", svc.Port, 5173)
	}
	if svc.Kind != "node" {
		t.Errorf("service kind = %q, want %q", svc.Kind, "node")
	}
	if svc.Confidence != ConfidenceHigh {
		t.Errorf("service confidence = %v, want %v", svc.Confidence, ConfidenceHigh)
	}
	if svc.Manifest != "./web/package.json" {
		t.Errorf("service manifest = %q, want %q", svc.Manifest, "./web/package.json")
	}
}

func TestScanDetectsRootNodePackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(report.Services))
	}
	svc := report.Services[0]
	if svc.Dir != "." {
		t.Errorf("service dir = %q, want .", svc.Dir)
	}
	if svc.Manifest != "package.json" {
		t.Errorf("service manifest = %q, want package.json", svc.Manifest)
	}
}

func TestScanDetectsGoCommandDirs(t *testing.T) {
	dir := t.TempDir()

	writeFile := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("failed to create dir for %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	writeFile(filepath.Join(dir, "go.mod"), "module example.com/myapp\n\ngo 1.23\n")
	writeFile(filepath.Join(dir, "cmd", "api", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(filepath.Join(dir, "cmd", "worker", "main.go"), "package main\n\nfunc main() {}\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(report.Services))
	}

	if report.Services[0].Name != "api" {
		t.Errorf("first service name = %q, want %q", report.Services[0].Name, "api")
	}
	if report.Services[0].Command != "go run ." {
		t.Errorf("first service command = %q, want %q", report.Services[0].Command, "go run .")
	}
	if report.Services[0].Dir != "./cmd/api" {
		t.Errorf("first service dir = %q, want %q", report.Services[0].Dir, "./cmd/api")
	}
	if report.Services[0].Kind != "go" {
		t.Errorf("first service kind = %q, want %q", report.Services[0].Kind, "go")
	}
	if report.Services[0].Confidence != ConfidenceMedium {
		t.Errorf("first service confidence = %v, want %v", report.Services[0].Confidence, ConfidenceMedium)
	}

	if report.Services[1].Name != "worker" {
		t.Errorf("second service name = %q, want %q", report.Services[1].Name, "worker")
	}
	if report.Services[1].Command != "go run ." {
		t.Errorf("second service command = %q, want %q", report.Services[1].Command, "go run .")
	}
	if report.Services[1].Dir != "./cmd/worker" {
		t.Errorf("second service dir = %q, want %q", report.Services[1].Dir, "./cmd/worker")
	}
}

func TestScanSkipsGoLibraryWithoutMain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/lib\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "lib.go"), "package lib\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 0 {
		t.Fatalf("expected no services, got %+v", report.Services)
	}
}

func TestScanSanitizesUnsafeGoCommandDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "api;touch bad", "main.go"), "package main\n\nfunc main() {}\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 0 {
		t.Fatalf("expected unsafe command dir to be skipped, got %+v", report.Services)
	}
}

func TestScanNestedNodePackage(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "apps", "web"), 0755); err != nil {
		t.Fatalf("failed to create apps/web dir: %v", err)
	}
	pkgJSON := `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"^5.0.0"}}`
	if err := os.WriteFile(filepath.Join(dir, "apps", "web", "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatalf("failed to write package.json: %v", err)
	}

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(report.Services))
	}

	svc := report.Services[0]
	if svc.Name != "web" {
		t.Errorf("service name = %q, want %q", svc.Name, "web")
	}
	if svc.Dir != "./apps/web" {
		t.Errorf("service dir = %q, want %q", svc.Dir, "./apps/web")
	}
	if svc.Manifest != "./apps/web/package.json" {
		t.Errorf("service manifest = %q, want %q", svc.Manifest, "./apps/web/package.json")
	}
	if svc.Port != 5173 {
		t.Errorf("service port = %d, want %d (vite default)", svc.Port, 5173)
	}
}

func TestScanSkipsWorktreesAndArchiveNodePackages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".worktrees", "feature", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, "archive", "old", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, "active", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected 1 active service, got %+v", report.Services)
	}
	if report.Services[0].Name != "active" {
		t.Fatalf("service name = %q, want active", report.Services[0].Name)
	}
}

func TestScanSkipsWorkspaceAggregatePackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", "package.json"), `{
  "workspaces":["client"],
  "scripts":{"dev":"concurrently \"npm run dev --workspace client\""},
  "devDependencies":{"concurrently":"latest"}
}`)
	writeFile(t, filepath.Join(dir, "app", "client", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected workspace child only, got %+v", report.Services)
	}
	if report.Services[0].Name != "client" {
		t.Fatalf("service name = %q, want client", report.Services[0].Name)
	}
}

func TestScanSkipsConcurrentlyAggregatePackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", "package.json"), `{
  "scripts":{"dev":"concurrently \"cd client && npm run dev\" \"cd server && npm run dev\""},
  "dependencies":{"concurrently":"latest"}
}`)
	writeFile(t, filepath.Join(dir, "app", "client", "package.json"), `{"scripts":{"dev":"vite"}}`)
	writeFile(t, filepath.Join(dir, "app", "server", "package.json"), `{"scripts":{"dev":"nodemon server.js"}}`)
	writeFile(t, filepath.Join(dir, "app", "server", ".env.example"), "PORT=5000\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 2 {
		t.Fatalf("expected child services only, got %+v", report.Services)
	}
	if report.Services[0].Name != "client" || report.Services[0].Port != 5173 {
		t.Fatalf("first service = %+v, want client on 5173", report.Services[0])
	}
}

func TestScanNextDefaultPort(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "client"), 0755); err != nil {
		t.Fatalf("failed to create client dir: %v", err)
	}
	pkgJSON := `{"scripts":{"dev":"next dev"},"dependencies":{"next":"14.0.0"}}`
	if err := os.WriteFile(filepath.Join(dir, "client", "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatalf("failed to write package.json: %v", err)
	}

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(report.Services))
	}

	svc := report.Services[0]
	if svc.Port != 3000 {
		t.Errorf("service port = %d, want %d (next default)", svc.Port, 3000)
	}
}

func TestScanDoesNotTreatVitestAsViteDevServer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{
  "scripts":{"dev":"next dev","test":"vitest run"},
  "dependencies":{"next":"14.0.0"},
  "devDependencies":{"vitest":"latest"}
}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %+v", report.Services)
	}
	if report.Services[0].Port != 3000 {
		t.Fatalf("port = %d, want Next default 3000", report.Services[0].Port)
	}
}

func TestScanDetectsPythonDjangoProject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api", "manage.py"), "#!/usr/bin/env python\n")
	writeFile(t, filepath.Join(dir, "api", ".env.example"), "PORT=8001\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(report.Services))
	}
	svc := report.Services[0]
	if svc.Name != "api" {
		t.Errorf("service name = %q, want %q", svc.Name, "api")
	}
	if svc.Command != "python manage.py runserver 0.0.0.0:8001" {
		t.Errorf("service command = %q, want django runserver", svc.Command)
	}
	if svc.Kind != "python" {
		t.Errorf("service kind = %q, want python", svc.Kind)
	}
	if svc.Port != 8001 {
		t.Errorf("service port = %d, want 8001", svc.Port)
	}
	if svc.Manifest != "./api/manage.py" {
		t.Errorf("service manifest = %q, want ./api/manage.py", svc.Manifest)
	}
}

func TestSuggestedYAMLIncludesPortsAndReadiness(t *testing.T) {
	services := []Service{
		{Name: "web", Dir: "./web", Command: "npm run dev", Port: 5173, Confidence: ConfidenceHigh},
	}

	yaml := renderSuggestedYAML(services)

	if !strings.Contains(yaml, "processes:") {
		t.Error("yaml should contain 'processes:'")
	}
	if !strings.Contains(yaml, "web:") {
		t.Error("yaml should contain 'web:'")
	}
	if !strings.Contains(yaml, "cmd: npm run dev") {
		t.Error("yaml should contain 'cmd: npm run dev'")
	}
	if !strings.Contains(yaml, "dir: ./web") {
		t.Error("yaml should contain 'dir: ./web'")
	}
	if !strings.Contains(yaml, "PORT: \"5173\"") {
		t.Error("yaml should contain 'PORT: \"5173\"'")
	}
	if !strings.Contains(yaml, "tcp: localhost:5173") {
		t.Error("yaml should contain 'tcp: localhost:5173'")
	}
}

func TestSuggestedYAMLAddsMergeForFrontendBackend(t *testing.T) {
	services := []Service{
		{Name: "backend", Dir: ".", Command: "go run .", Port: 3001, Kind: "go", Confidence: ConfidenceMedium},
		{Name: "frontend", Dir: "./client", Command: "npm run dev", Port: 5173, Kind: "node", Confidence: ConfidenceHigh},
	}

	yaml := renderSuggestedYAML(services)

	if !strings.Contains(yaml, "merge:") {
		t.Error("yaml should contain 'merge:' for frontend+backend detected")
	}
	if !strings.Contains(yaml, "client: 5173") {
		t.Error("yaml should contain 'client: 5173'")
	}
	if !strings.Contains(yaml, "server: 3001") {
		t.Error("yaml should contain 'server: 3001'")
	}
}

func TestSuggestedYAMLMakesDuplicateProcessNamesUnique(t *testing.T) {
	services := []Service{
		{Name: "web", Dir: "./apps/web", Command: "npm run dev", Port: 5173, Kind: "node", Confidence: ConfidenceHigh},
		{Name: "web", Dir: "./packages/web", Command: "npm run dev", Port: 5174, Kind: "node", Confidence: ConfidenceHigh},
	}

	yaml := renderSuggestedYAML(services)

	if !strings.Contains(yaml, "web:") {
		t.Error("yaml should contain first web process")
	}
	if !strings.Contains(yaml, "packages-web:") {
		t.Error("yaml should contain unique process name for duplicate web service")
	}
}

func TestRunMakesReportedServiceNamesUnique(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, "packages", "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 2 {
		t.Fatalf("expected 2 services, got %+v", report.Services)
	}
	if report.Services[0].Name == report.Services[1].Name {
		t.Fatalf("service names should be unique, got %+v", report.Services)
	}
}

func TestSuggestedYAMLDoesNotMergeSingleAPIOnPort3000(t *testing.T) {
	services := []Service{
		{Name: "api", Dir: ".", Command: "npm run dev", Port: 3000, Kind: "node", Confidence: ConfidenceHigh},
	}

	yaml := renderSuggestedYAML(services)

	if strings.Contains(yaml, "merge:") {
		t.Fatalf("yaml should not contain merge for single api service on 3000:\n%s", yaml)
	}
}

func TestSuggestedYAMLDoesNotMergeAmbiguousMonorepo(t *testing.T) {
	services := []Service{
		{Name: "web", Dir: "./web", Command: "npm run dev", Port: 5173, Kind: "node", Confidence: ConfidenceHigh},
		{Name: "admin", Dir: "./admin", Command: "npm run dev", Port: 5174, Kind: "node", Confidence: ConfidenceHigh},
		{Name: "api", Dir: "./api", Command: "npm run dev", Port: 3000, Kind: "node", Confidence: ConfidenceHigh},
		{Name: "server", Dir: "./server", Command: "npm run dev", Port: 5000, Kind: "node", Confidence: ConfidenceHigh},
	}

	yaml := renderSuggestedYAML(services)

	if strings.Contains(yaml, "merge:") {
		t.Fatalf("yaml should not contain merge for ambiguous multi-frontend/multi-backend repo:\n%s", yaml)
	}
}

func TestExistingConfigReportsMissingDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), "processes:\n  api:\n    cmd: go run .\n    dir: ./missing\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(report, codeMissingDir) {
		t.Fatalf("expected missing_dir finding, got %+v", report.Findings)
	}
}

func TestExistingConfigReportsOtherFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    env:
      PORT: "3000"
  worker:
    cmd: go run ./worker
    depends_on:
      - api
  web:
    cmd: npm run dev
    ready_when:
      tcp: localhost:3000
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []string{codeDuplicatePort, codeDependencyWithoutReadiness} {
		if !hasFinding(report, code) {
			t.Fatalf("expected %s finding, got %+v", code, report.Findings)
		}
	}
}

func TestExistingConfigReportsMissingEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    env_file: ./missing.env
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(report, codeMissingEnvFile) {
		t.Fatalf("expected missing_env_file finding, got %+v", report.Findings)
	}
}

func TestExistingConfigContinuesDiagnosticsAfterEnvFileLoadError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    dir: ./missing
    env:
      PORT: "3000"
  web:
    cmd: npm run dev
    ready_when:
      tcp: localhost:3000
  worker:
    cmd: go run ./worker
    depends_on:
      - api
    env_file: ./missing.env
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []string{codeMissingEnvFile, codeInvalidConfig, codeMissingDir, codeDuplicatePort, codeDependencyWithoutReadiness} {
		if !hasFinding(report, code) {
			t.Fatalf("expected %s finding, got %+v", code, report.Findings)
		}
	}
}

func TestExistingConfigReportsMissingMergePort(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `merge:
  client: 5173
  server: 3001

processes:
  web:
    cmd: npm run dev
  api:
    cmd: go run .
`)
	old := mergePortInPath
	mergePortInPath = func() bool { return false }
	t.Cleanup(func() { mergePortInPath = old })

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(report, codeMissingMergePort) {
		t.Fatalf("expected missing_merge_port finding, got %+v", report.Findings)
	}
}

func TestProcessPortFallsBackToReadyWhen(t *testing.T) {
	port := processPort(config.Process{
		Env: map[string]string{"PORT": "not-a-number"},
		ReadyWhen: &config.ReadyWhen{
			TCP: "localhost:3000",
		},
	})
	if port != 3000 {
		t.Fatalf("port = %d, want 3000", port)
	}
}

func TestWriteCreatesConfigOnlyWhenMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)

	report, err := Run(Options{Root: dir, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.GeneratedPath == "" {
		t.Fatal("expected generated path")
	}
	if _, err := os.Stat(filepath.Join(dir, "proc-compose.yml")); err != nil {
		t.Fatal(err)
	}

	_, err = Run(Options{Root: dir, Write: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
	}
}

func TestWriteRefusesWhenYamlConfigExists(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, "proc-compose.yaml"), "processes:\n  web:\n    cmd: npm run dev\n")

	report, err := Run(Options{Root: dir, Write: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
	}
	if report.ConfigPath != filepath.Join(dir, "proc-compose.yaml") {
		t.Fatalf("config path = %q, want existing yaml", report.ConfigPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "proc-compose.yml")); !os.IsNotExist(err) {
		t.Fatalf("proc-compose.yml should not be created when yaml exists: %v", err)
	}
}

func TestWriteRefusesExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	target := filepath.Join(dir, "target.yml")
	writeFile(t, target, "processes: {}\n")
	if err := os.Symlink(target, filepath.Join(dir, "proc-compose.yml")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := Run(Options{Root: dir, Write: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func hasFinding(report *Report, code string) bool {
	for _, finding := range report.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
