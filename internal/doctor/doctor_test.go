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

func TestScanUsesPackageManagerRunnerAndScriptPort(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{
  "scripts":{"dev":"vite --host 0.0.0.0 --port 4444"},
  "devDependencies":{"vite":"latest"}
}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected 1 service, got %+v", report.Services)
	}
	if report.Services[0].Command != "pnpm dev" {
		t.Fatalf("command = %q, want pnpm dev", report.Services[0].Command)
	}
	if report.Services[0].Port != 4444 {
		t.Fatalf("port = %d, want script --port", report.Services[0].Port)
	}
}

func TestScanNestedNodePackageInheritsWorkspaceLockfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	writeFile(t, filepath.Join(dir, "apps", "web", "package.json"), `{
  "scripts":{"dev":"vite"},
  "devDependencies":{"vite":"latest"}
}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected nested service, got %+v", report.Services)
	}
	if report.Services[0].Command != "pnpm dev" {
		t.Fatalf("command = %q, want pnpm dev inherited from root lockfile", report.Services[0].Command)
	}
}

func TestScanDetectsRootWorkspacePackageWithRunnableScript(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "yarn.lock"), "# yarn lockfile\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{
  "workspaces":["packages/*"],
  "scripts":{"dev":"next dev"},
  "dependencies":{"next":"14.0.0"}
}`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected root workspace service, got %+v", report.Services)
	}
	if report.Services[0].Command != "yarn dev" {
		t.Fatalf("command = %q, want yarn dev", report.Services[0].Command)
	}
	if report.Services[0].Port != 3000 {
		t.Fatalf("port = %d, want Next default", report.Services[0].Port)
	}
}

func TestScanDetectsFrameworkDefaultPorts(t *testing.T) {
	tests := []struct {
		name string
		pkg  string
		port int
	}{
		{name: "svelte kit", pkg: `{"scripts":{"dev":"svelte-kit dev"},"devDependencies":{"@sveltejs/kit":"latest"}}`, port: 5173},
		{name: "astro", pkg: `{"scripts":{"dev":"astro dev"},"devDependencies":{"astro":"latest"}}`, port: 4321},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "package.json"), tt.pkg)
			report, err := Run(Options{Root: dir})
			if err != nil {
				t.Fatalf("Run failed: %v", err)
			}
			if len(report.Services) != 1 {
				t.Fatalf("expected 1 service, got %+v", report.Services)
			}
			if report.Services[0].Port != tt.port {
				t.Fatalf("port = %d, want %d", report.Services[0].Port, tt.port)
			}
		})
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
	if report.Services[0].Command != "go run ./cmd/api" {
		t.Errorf("first service command = %q, want %q", report.Services[0].Command, "go run ./cmd/api")
	}
	if report.Services[0].Dir != "." {
		t.Errorf("first service dir = %q, want .", report.Services[0].Dir)
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
	if report.Services[1].Command != "go run ./cmd/worker" {
		t.Errorf("second service command = %q, want %q", report.Services[1].Command, "go run ./cmd/worker")
	}
	if report.Services[1].Dir != "." {
		t.Errorf("second service dir = %q, want .", report.Services[1].Dir)
	}
}

func TestScanDetectsNestedGoCommandDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "services", "api", "main.go"), "package main\n\nfunc main() {}\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("expected nested Go service, got %+v", report.Services)
	}
	if report.Services[0].Name != "api" {
		t.Fatalf("name = %q, want api", report.Services[0].Name)
	}
	if report.Services[0].Command != "go run ./cmd/services/api" {
		t.Fatalf("command = %q, want go run ./cmd/services/api", report.Services[0].Command)
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

func TestScanSkipsGoCommandWithUnsafeAncestorDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "api;touch bad", "server", "main.go"), "package main\n\nfunc main() {}\n")

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(report.Services) != 0 {
		t.Fatalf("expected unsafe ancestor dir to be skipped, got %+v", report.Services)
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

func TestScanDetectsPythonASGIAndFlaskCommands(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		want string
		port int
	}{
		{name: "uvicorn pyproject", file: "pyproject.toml", body: "[project]\ndependencies = ['uvicorn']\n", want: "python -m uvicorn main:app --host 0.0.0.0 --port 9000 --reload", port: 9000},
		{name: "flask requirements", file: "requirements.txt", body: "Flask==3.0.0\n", want: "python -m flask --app main run --host 0.0.0.0 --port 5000", port: 5000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "main.py"), "app = object()\n")
			writeFile(t, filepath.Join(dir, tt.file), tt.body)
			if tt.port == 9000 {
				writeFile(t, filepath.Join(dir, ".env.local"), "PORT=9000\n")
			}
			report, err := Run(Options{Root: dir})
			if err != nil {
				t.Fatalf("Run failed: %v", err)
			}
			if len(report.Services) != 1 {
				t.Fatalf("expected 1 service, got %+v", report.Services)
			}
			if report.Services[0].Command != tt.want {
				t.Fatalf("command = %q, want %q", report.Services[0].Command, tt.want)
			}
			if report.Services[0].Port != tt.port {
				t.Fatalf("port = %d, want %d", report.Services[0].Port, tt.port)
			}
		})
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

// TestTeardownPortIsNotADuplicate: a teardown process's PORT names the
// service it talks to after the stack stopped; it never binds it.
func TestTeardownPortIsNotADuplicate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    env:
      PORT: "3000"
  cleanup:
    mode: teardown
    cmd: go run ./cleanup
    env:
      PORT: "3000"
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFinding(report, codeDuplicatePort) {
		t.Fatalf("teardown PORT reported as a duplicate: %+v", report.Findings)
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
	// The finding about the config on disk keeps an empty detail; the
	// suggested config carries its own detail.
	found := findingsByCode(report, codeMissingMergePort)
	if len(found) != 1 {
		t.Fatalf("expected exactly one missing_merge_port finding, got %+v", found)
	}
	if found[0].Detail != "" {
		t.Fatalf("existing-config finding detail = %q, want empty", found[0].Detail)
	}
}

func TestExistingConfigWarnsOnAlwaysRestartWithoutCapAndMissingTunnel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  tunnel:
    cmd: tunnel http 8080 --name demo
    restart: always
`)
	old := tunnelInPath
	tunnelInPath = func() bool { return false }
	t.Cleanup(func() { tunnelInPath = old })

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"uncapped_always_restart", "missing_tunnel"} {
		if !hasFinding(report, code) {
			t.Fatalf("expected %s finding, got %+v", code, report.Findings)
		}
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

// findingsByCode returns the findings with the given code, in report order.
func findingsByCode(report *Report, code string) []Finding {
	var found []Finding
	for _, finding := range report.Findings {
		if finding.Code == code {
			found = append(found, finding)
		}
	}
	return found
}

// findingSignature renders the findings of a report as a comparable string so
// two runs can be compared for determinism.
func findingSignature(report *Report) string {
	parts := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		parts = append(parts, finding.Code+"|"+finding.Message+"|"+finding.Detail)
	}
	return strings.Join(parts, "\n")
}

// stubMergePortInPath replaces the merge-port PATH probe for one test.
func stubMergePortInPath(t *testing.T, present bool) {
	t.Helper()
	old := mergePortInPath
	mergePortInPath = func() bool { return present }
	t.Cleanup(func() { mergePortInPath = old })
}

// writeMergeProject writes a project with one frontend and one backend so the
// doctor's suggestion carries a merge: section.
func writeMergeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "web", "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, "api", "package.json"), `{"scripts":{"dev":"node server.js"}}`)
	writeFile(t, filepath.Join(dir, "api", ".env.example"), "PORT=3001\n")
	return dir
}

// TestExistingConfigSkipsMissingDirForRuntimeTemplate: a dir that still
// holds a runtime template is expanded when the session starts, so the doctor
// must not assert that it does not exist.
func TestExistingConfigSkipsMissingDirForRuntimeTemplate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    dir: ${config.dir}
  web:
    cmd: npm run dev
    dir: ${session.dir}
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if found := findingsByCode(report, codeMissingDir); len(found) != 0 {
		t.Fatalf("expected no missing_dir finding for runtime templates, got %+v", found)
	}
}

// TestSuggestedConfigReportsMissingMergePort: the config the doctor suggests
// is what the user is told to run, so a missing merge-port binary in it is
// reported even when no config exists.
func TestSuggestedConfigReportsMissingMergePort(t *testing.T) {
	dir := writeMergeProject(t)
	stubMergePortInPath(t, false)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	found := findingsByCode(report, codeMissingMergePort)
	if len(found) != 1 {
		t.Fatalf("expected exactly one missing_merge_port finding, got %+v", report.Findings)
	}
	if found[0].Detail != "suggested config" {
		t.Fatalf("finding detail = %q, want %q", found[0].Detail, "suggested config")
	}
	if !strings.Contains(found[0].Message, "suggested config") {
		t.Fatalf("finding message = %q, want it to name the suggested config", found[0].Message)
	}
}

// TestSuggestedConfigHasNoMergePortFindingWhenBinaryIsOnPath: the check
// reports a missing binary, not the mere presence of a merge: section.
func TestSuggestedConfigHasNoMergePortFindingWhenBinaryIsOnPath(t *testing.T) {
	dir := writeMergeProject(t)
	stubMergePortInPath(t, true)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if found := findingsByCode(report, codeMissingMergePort); len(found) != 0 {
		t.Fatalf("expected no missing_merge_port finding, got %+v", found)
	}
}

// TestMergePortFindingsAreDeduplicatedPerSubject: an existing config and the
// suggested config are two subjects, so each may carry its own finding, but
// neither may carry more than one.
func TestMergePortFindingsAreDeduplicatedPerSubject(t *testing.T) {
	dir := writeMergeProject(t)
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `merge:
  client: 5173
  server: 3001

processes:
  web:
    cmd: npm run dev
  api:
    cmd: go run .
`)
	stubMergePortInPath(t, false)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	found := findingsByCode(report, codeMissingMergePort)
	if len(found) != 2 {
		t.Fatalf("expected one finding per subject, got %+v", found)
	}
	byDetail := map[string]int{}
	for _, finding := range found {
		byDetail[finding.Detail]++
	}
	if byDetail[""] != 1 || byDetail["suggested config"] != 1 {
		t.Fatalf("expected one finding per subject detail, got %+v", byDetail)
	}
}

// TestUnparseableSuggestedConfigIsIgnored: a suggestion that does not parse
// must not produce a finding, an invalid_config finding, or an error.
func TestUnparseableSuggestedConfigIsIgnored(t *testing.T) {
	report := &Report{Findings: []Finding{}, SuggestedYAML: "processes: [\n"}

	checkSuggestedConfig(report)

	if len(report.Findings) != 0 {
		t.Fatalf("expected no findings for an unparseable suggestion, got %+v", report.Findings)
	}
}

// TestDoctorFindingsAreDeterministic: the doctor output is a diagnostic
// artifact consumed by humans and CI, so repeated runs must agree.
func TestDoctorFindingsAreDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  alpha:
    cmd: echo alpha
    dir: ./missing-alpha
    env:
      PORT: "3000"
  bravo:
    cmd: echo bravo
    dir: ./missing-bravo
    env:
      PORT: "3000"
  charlie:
    cmd: echo charlie
    env_file: ./missing-charlie.env
`)

	var first string
	for i := 0; i < 40; i++ {
		report, err := Run(Options{Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		signature := findingSignature(report)
		if i == 0 {
			first = signature
			continue
		}
		if signature != first {
			t.Fatalf("run %d produced a different finding sequence:\n%s\nfirst:\n%s", i, signature, first)
		}
	}
}

// TestMissingEnvFileFindingsAreDeterministic: precheckEnvFiles is the only
// producer of missing_env_file findings, so its order must be stable too.
func TestMissingEnvFileFindingsAreDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  alpha:
    cmd: echo alpha
    env_file: ./missing-alpha.env
  bravo:
    cmd: echo bravo
    env_file: ./missing-bravo.env
  charlie:
    cmd: echo charlie
    env_file: ./missing-charlie.env
`)

	var first string
	for i := 0; i < 40; i++ {
		report, err := Run(Options{Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		found := findingsByCode(report, codeMissingEnvFile)
		var order []string
		for _, finding := range found {
			order = append(order, finding.Message)
		}
		signature := strings.Join(order, "\n")
		if i == 0 {
			first = signature
			continue
		}
		if signature != first {
			t.Fatalf("run %d produced a different missing_env_file order:\n%s\nfirst:\n%s", i, signature, first)
		}
	}
	if first == "" {
		t.Fatal("expected missing_env_file findings")
	}
}

// TestDuplicatePortFindingsUseLexicographicallyFirstProcesses: the set and
// count of duplicate-port findings does not change with a deterministic
// iteration order, but the reported pair does — it is now the two
// lexicographically-first processes.
//
// The fixture lists its processes in reverse-sorted order deliberately. The
// doctor reads the config into a map, and Go randomises a map's iteration
// order; the pre-fix code ranged over that map, so the process that owned the
// port — and with it the reported pair — depended on the iteration order. A
// map built from a reverse-sorted document is only ever iterated in rotations
// of that document order, and no rotation yields the sorted pair sequence, so
// the pre-fix code fails this test on every run instead of only when the
// random order happens to differ.
//
// The check is repeated as well: every run must yield the same finding
// sequence, so a report that is not reproducible fails the test too.
func TestDuplicatePortFindingsUseLexicographicallyFirstProcesses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  charlie:
    cmd: echo charlie
    env:
      PORT: "3000"
  bravo:
    cmd: echo bravo
    env:
      PORT: "3000"
  alpha:
    cmd: echo alpha
    env:
      PORT: "3000"
`)

	want := []string{
		`processes "alpha" and "bravo" both use port 3000`,
		`processes "alpha" and "charlie" both use port 3000`,
	}
	const runs = 40
	for run := 0; run < runs; run++ {
		report, err := Run(Options{Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		found := findingsByCode(report, codeDuplicatePort)
		if len(found) != 2 {
			t.Fatalf("run %d: expected 2 duplicate_port findings, got %+v", run, found)
		}
		for i, message := range want {
			if found[i].Message != message {
				t.Fatalf("run %d: finding %d message = %q, want %q", run, i, found[i].Message, message)
			}
		}
	}
}

func TestServiceDependencyWithoutReadinessWarns(t *testing.T) {
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
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(report, codeDependencyWithoutReadiness) {
		t.Fatalf("expected dependency_without_readiness warning for service-mode dep, got %+v", report.Findings)
	}
}

func TestTaskDependencyWithoutReadinessNoWarning(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  migrate:
    cmd: go run ./migrate
    mode: task
  app:
    cmd: go run .
    depends_on:
      - migrate
`)

	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFinding(report, codeDependencyWithoutReadiness) {
		t.Fatalf("expected no dependency_without_readiness warning for task-mode dep, got %+v", report.Findings)
	}
}

func TestServiceDependencyWithReadinessNoWarning(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc-compose.yml"), `processes:
  api:
    cmd: go run .
    ready_when:
      tcp: localhost:3000
  worker:
    cmd: go run ./worker
    depends_on:
      - api
`)
	report, err := Run(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if hasFinding(report, codeDependencyWithoutReadiness) {
		t.Fatalf("expected no dependency_without_readiness warning when dep has ready_when, got %+v", report.Findings)
	}
}
