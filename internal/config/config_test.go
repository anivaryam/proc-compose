package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildMergeCmd_SimpleMode(t *testing.T) {
	tests := []struct {
		name    string
		merge   Merge
		want    string
		wantErr string
	}{
		{
			name:  "basic client and server",
			merge: Merge{Client: 3000, Server: 3001},
			want:  "merge-port --client 3000 --server 3001 --port 8080",
		},
		{
			name:  "custom port",
			merge: Merge{Client: 3000, Server: 3001, Port: 9000},
			want:  "merge-port --client 3000 --server 3001 --port 9000",
		},
		{
			name:  "single api prefix",
			merge: Merge{Client: 3000, Server: 3001, ApiPrefixes: []string{"/api/v1"}},
			want:  "merge-port --client 3000 --server 3001 --api-prefix /api/v1 --port 8080",
		},
		{
			name:  "multiple api prefixes",
			merge: Merge{Client: 3000, Server: 3001, ApiPrefixes: []string{"/api", "/auth", "/health"}},
			want:  "merge-port --client 3000 --server 3001 --api-prefix /api --api-prefix /auth --api-prefix /health --port 8080",
		},
		{
			name:    "missing client",
			merge:   Merge{Server: 3001},
			wantErr: "merge.client",
		},
		{
			name:    "missing server",
			merge:   Merge{Client: 3000},
			wantErr: "merge.server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT", "") // isolate from ambient PORT env var
			got, err := buildMergeCmd(&tt.merge)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMergeCmd_RouteMode(t *testing.T) {
	tests := []struct {
		name  string
		merge Merge
		want  string
	}{
		{
			name:  "bare ports",
			merge: Merge{Routes: []string{"/api=3001", "/=3000"}},
			want:  "merge-port --route /api=3001 --route /=3000 --port 8080",
		},
		{
			name:  "full URLs",
			merge: Merge{Routes: []string{"/api=http://api.local:3001", "/=3000"}, Port: 9000},
			want:  "merge-port --route /api=http://api.local:3001 --route /=3000 --port 9000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT", "") // isolate from ambient PORT env var
			got, err := buildMergeCmd(&tt.merge)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMergeCmd_MutualExclusivity(t *testing.T) {
	tests := []struct {
		name  string
		merge Merge
	}{
		{
			name:  "routes with client",
			merge: Merge{Client: 3000, Routes: []string{"/api=3001"}},
		},
		{
			name:  "routes with server",
			merge: Merge{Server: 3001, Routes: []string{"/api=3001"}},
		},
		{
			name:  "routes with api_prefixes",
			merge: Merge{ApiPrefixes: []string{"/api"}, Routes: []string{"/api=3001"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildMergeCmd(&tt.merge)
			if err == nil {
				t.Fatal("expected mutual exclusivity error, got nil")
			}
			if !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("expected error containing 'cannot be combined', got %q", err.Error())
			}
		})
	}
}

func TestBuildMergeCmd_Empty(t *testing.T) {
	_, err := buildMergeCmd(&Merge{})
	if err == nil {
		t.Fatal("expected error for empty merge config, got nil")
	}
}

func TestLoad_MergeSimpleMode(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 3001

processes:
  app:
    cmd: npm start
`
	cfg := loadFromString(t, yaml)
	proc, ok := cfg.Processes["merge-port"]
	if !ok {
		t.Fatal("expected merge-port process to be injected")
	}
	if !strings.Contains(proc.Cmd, "--client 5173") {
		t.Fatalf("expected --client 5173 in cmd, got %q", proc.Cmd)
	}
	if !strings.Contains(proc.Cmd, "--server 3001") {
		t.Fatalf("expected --server 3001 in cmd, got %q", proc.Cmd)
	}
	if proc.Restart != "on-failure" {
		t.Fatalf("expected restart on-failure, got %q", proc.Restart)
	}
}

func TestLoad_MergeWithApiPrefixes(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 5000
  api_prefixes:
    - /api
    - /health
    - /uploads

processes:
  app:
    cmd: npm start
`
	cfg := loadFromString(t, yaml)
	proc := cfg.Processes["merge-port"]
	if !strings.Contains(proc.Cmd, "--api-prefix /api") {
		t.Fatalf("expected --api-prefix /api in cmd, got %q", proc.Cmd)
	}
	if !strings.Contains(proc.Cmd, "--api-prefix /health") {
		t.Fatalf("expected --api-prefix /health in cmd, got %q", proc.Cmd)
	}
	if !strings.Contains(proc.Cmd, "--api-prefix /uploads") {
		t.Fatalf("expected --api-prefix /uploads in cmd, got %q", proc.Cmd)
	}
}

func TestLoad_MergeRouteMode(t *testing.T) {
	yaml := `
merge:
  routes:
    - /api=3001
    - /auth=3002
    - /=3000

processes:
  app:
    cmd: npm start
`
	cfg := loadFromString(t, yaml)
	proc := cfg.Processes["merge-port"]
	if !strings.Contains(proc.Cmd, "--route /api=3001") {
		t.Fatalf("expected --route /api=3001 in cmd, got %q", proc.Cmd)
	}
	if !strings.Contains(proc.Cmd, "--route /auth=3002") {
		t.Fatalf("expected --route /auth=3002 in cmd, got %q", proc.Cmd)
	}
	if !strings.Contains(proc.Cmd, "--route /=3000") {
		t.Fatalf("expected --route /=3000 in cmd, got %q", proc.Cmd)
	}
}

func TestLoad_MergePortDependsOnUpstreams(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 3001

processes:
  client:
    cmd: npm run dev
    env:
      PORT: "5173"
  server:
    cmd: go run .
    env:
      PORT: "3001"
`
	cfg := loadFromString(t, yaml)
	mp := cfg.Processes["merge-port"]
	want := []string{"client", "server"}
	if !reflect.DeepEqual(mp.DependsOn, want) {
		t.Errorf("merge-port depends_on = %v, want %v", mp.DependsOn, want)
	}

	// Upstreams without ready_when get a TCP probe wired automatically.
	if rw := cfg.Processes["client"].ReadyWhen; rw == nil || rw.TCP != "localhost:5173" {
		t.Errorf("client ready_when.tcp = %v, want localhost:5173", rw)
	}
	if rw := cfg.Processes["server"].ReadyWhen; rw == nil || rw.TCP != "localhost:3001" {
		t.Errorf("server ready_when.tcp = %v, want localhost:3001", rw)
	}
}

func TestLoad_MergePortPreservesUserReadyWhen(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 3001

processes:
  client:
    cmd: npm run dev
    env:
      PORT: "5173"
  server:
    cmd: go run .
    env:
      PORT: "3001"
    ready_when:
      http: http://localhost:3001/health
`
	cfg := loadFromString(t, yaml)
	// User's HTTP probe must win over the auto-injected TCP probe.
	rw := cfg.Processes["server"].ReadyWhen
	if rw == nil || rw.HTTP != "http://localhost:3001/health" {
		t.Errorf("server ready_when = %+v, want user's HTTP probe", rw)
	}
	if rw != nil && rw.TCP != "" {
		t.Errorf("auto TCP probe should not override user HTTP probe; got TCP=%q", rw.TCP)
	}
	// Dependency edge still added.
	mp := cfg.Processes["merge-port"]
	if !contains(mp.DependsOn, "server") {
		t.Errorf("merge-port should depend on server even when user set ready_when; got %v", mp.DependsOn)
	}
}

func TestLoad_MergePortRouteMode_DependsOnAllBackings(t *testing.T) {
	yaml := `
merge:
  routes:
    - /api=3001
    - /auth=3002
    - /=3000

processes:
  api:
    cmd: go run ./api
    env:
      PORT: "3001"
  auth:
    cmd: go run ./auth
    env:
      PORT: "3002"
  web:
    cmd: npm run dev
    env:
      PORT: "3000"
`
	cfg := loadFromString(t, yaml)
	mp := cfg.Processes["merge-port"]
	want := []string{"api", "auth", "web"}
	if !reflect.DeepEqual(mp.DependsOn, want) {
		t.Errorf("merge-port depends_on = %v, want %v (sorted)", mp.DependsOn, want)
	}
	for name, port := range map[string]string{
		"api":  "localhost:3001",
		"auth": "localhost:3002",
		"web":  "localhost:3000",
	} {
		rw := cfg.Processes[name].ReadyWhen
		if rw == nil || rw.TCP != port {
			t.Errorf("%s ready_when.tcp = %v, want %s", name, rw, port)
		}
	}
}

func TestLoad_MergePortClientProcessOverride(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 3001
  client_process: webapp

processes:
  webapp:
    cmd: npm run dev
    env:
      PORT: "5173"
  server:
    cmd: go run .
    env:
      PORT: "3001"
`
	cfg := loadFromString(t, yaml)
	mp := cfg.Processes["merge-port"]
	want := []string{"server", "webapp"}
	if !reflect.DeepEqual(mp.DependsOn, want) {
		t.Errorf("merge-port depends_on = %v, want %v", mp.DependsOn, want)
	}
}

func TestLoad_MergePortNoMatchingUpstreamSkipsDep(t *testing.T) {
	// When merge.server points at a port no managed process owns, we
	// can't inject a dependency — the user is presumably proxying to
	// something external. This must not be a hard error.
	yaml := `
merge:
  client: 5173
  server: 9999

processes:
  client:
    cmd: npm run dev
    env:
      PORT: "5173"
`
	cfg := loadFromString(t, yaml)
	mp := cfg.Processes["merge-port"]
	if !contains(mp.DependsOn, "client") {
		t.Errorf("expected client in depends_on, got %v", mp.DependsOn)
	}
	for _, n := range mp.DependsOn {
		if n == "server" || n == "merge-port" {
			t.Errorf("unexpected dep %q in %v", n, mp.DependsOn)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func loadFromString(t *testing.T, content string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	return cfg
}

// ─── parseEnvFile ─────────────────────────────────────────────────────────────

func TestParseEnvFile(t *testing.T) {
	content := `
# comment
VITE_API_BASE_URL=http://192.168.1.45:3000/api
VITE_APP_NAME=My App
QUOTED_DOUBLE="hello world"
QUOTED_SINGLE='foo bar'
EMPTY=
`
	dir := t.TempDir()
	f := filepath.Join(dir, ".env.example")
	os.WriteFile(f, []byte(content), 0644)

	got, err := parseEnvFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := map[string]string{
		"VITE_API_BASE_URL": "http://192.168.1.45:3000/api",
		"VITE_APP_NAME":     "My App",
		"QUOTED_DOUBLE":     "hello world",
		"QUOTED_SINGLE":     "foo bar",
		"EMPTY":             "",
	}
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("key %q: got %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["# comment"]; ok {
		t.Error("comment should not be parsed as key")
	}
}

func TestParseEnvFile_Missing(t *testing.T) {
	_, err := parseEnvFile("/nonexistent/.env")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestLoad_EnvFile_Missing(t *testing.T) {
	yaml := `
processes:
  backend:
    cmd: node index.js
    env_file: .env.nonexistent
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing env_file, got nil")
	}
	if !strings.Contains(err.Error(), "env file") {
		t.Fatalf("expected error about env file, got: %v", err)
	}
}

// ─── rebaseURL ────────────────────────────────────────────────────────────────

func TestRebaseURL(t *testing.T) {
	base := "http://localhost:8080"
	cases := []struct {
		exampleVal string
		want       string
	}{
		{"http://192.168.1.45:3000/api", "http://localhost:8080/api"},
		{"https://api.example.com/api/v2", "http://localhost:8080/api/v2"},
		{"http://example.com", "http://localhost:8080"},
		{"http://example.com/", "http://localhost:8080"},
		{"", "http://localhost:8080"},
		{"not-a-url", "http://localhost:8080"},
	}
	for _, c := range cases {
		got := rebaseURL(c.exampleVal, base)
		if got != c.want {
			t.Errorf("rebaseURL(%q, %q) = %q, want %q", c.exampleVal, base, got, c.want)
		}
	}
}

// ─── topPrefix ────────────────────────────────────────────────────────────────

func TestTopPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/employees", "/api"},
		{"/api", "/api"},
		{"/health", "/health"},
		{"/", "/"},
		{"api", "/api"}, // missing leading slash still extracts correctly
	}
	for _, c := range cases {
		got := topPrefix(c.in)
		if got != c.want {
			t.Errorf("topPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─── scanRoutes ───────────────────────────────────────────────────────────────

func TestScanRoutes_Express(t *testing.T) {
	src := `
const express = require('express')
const app = express()

app.use('/api', apiRouter)
app.get('/health', healthHandler)
app.use('/uploads', express.static('public'))
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.js"), []byte(src), 0644)

	got := scanRoutes(dir)
	want := []string{"/api", "/health", "/uploads"}
	if !equalSlices(got, want) {
		t.Errorf("scanRoutes got %v, want %v", got, want)
	}
}

func TestScanRoutes_TypeScript(t *testing.T) {
	src := `
import express from 'express'
const app = express()
app.use('/api', router)
app.get('/docs', docsHandler)
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "server.ts"), []byte(src), 0644)

	got := scanRoutes(dir)
	want := []string{"/api", "/docs"}
	if !equalSlices(got, want) {
		t.Errorf("scanRoutes got %v, want %v", got, want)
	}
}

func TestScanRoutes_IgnoresSubRouterPaths(t *testing.T) {
	// auth.routes.ts defines router.post('/login') which is mounted under
	// '/api/auth'. The scanner must NOT add '/login' as a top-level prefix.
	dir := t.TempDir()
	// Main entry — only top-level app routes should be detected.
	os.WriteFile(filepath.Join(dir, "app.js"), []byte(`
app.use('/api', apiRouter)
app.get('/health', h)
`), 0644)
	// Sub-router file — router.* paths must be ignored.
	routesDir := filepath.Join(dir, "routes")
	os.MkdirAll(routesDir, 0755)
	os.WriteFile(filepath.Join(routesDir, "auth.routes.ts"), []byte(`
router.post('/login', loginHandler)
router.post('/logout', logoutHandler)
router.get('/profile', profileHandler)
`), 0644)

	got := scanRoutes(dir)
	for _, p := range got {
		if p == "/login" || p == "/logout" || p == "/profile" {
			t.Errorf("scanRoutes incorrectly detected sub-router path %q as top-level prefix", p)
		}
	}
	want := []string{"/api", "/health"}
	if !equalSlices(got, want) {
		t.Errorf("scanRoutes got %v, want %v", got, want)
	}
}

func TestScanRoutes_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	// Real source file
	os.WriteFile(filepath.Join(dir, "app.js"), []byte(`app.use('/api', h)`), 0644)
	// Test file — should be ignored
	os.WriteFile(filepath.Join(dir, "app.test.js"), []byte(`app.use('/test-only', h)`), 0644)

	got := scanRoutes(dir)
	for _, p := range got {
		if p == "/test-only" {
			t.Error("scanRoutes should not include routes from test files")
		}
	}
}

func TestScanRoutes_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules", "some-pkg")
	os.MkdirAll(nm, 0755)
	os.WriteFile(filepath.Join(nm, "index.js"), []byte(`app.use('/internal', h)`), 0644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte(`app.use('/api', h)`), 0644)

	got := scanRoutes(dir)
	for _, p := range got {
		if p == "/internal" {
			t.Error("scanRoutes should not scan node_modules")
		}
	}
}

// ─── injectClientAPIEnv ───────────────────────────────────────────────────────

func TestInjectClientAPIEnv(t *testing.T) {
	// Set up a temp client dir with .env.example
	clientDir := t.TempDir()
	envExample := "VITE_API_BASE_URL=http://192.168.1.45:3000/api\nVITE_APP_NAME=Test\n"
	os.WriteFile(filepath.Join(clientDir, ".env.example"), []byte(envExample), 0644)

	procs := map[string]Process{
		"client": {Cmd: "npm run dev"},
	}
	m := &Merge{Client: 5173, Server: 3000, Port: 8080}

	injectClientAPIEnv(procs, "client", clientDir, m)

	got := procs["client"].Env["VITE_API_BASE_URL"]
	want := "http://localhost:8080/api"
	if got != want {
		t.Errorf("VITE_API_BASE_URL = %q, want %q", got, want)
	}
	// Non-API vars should not be injected.
	if _, ok := procs["client"].Env["VITE_APP_NAME"]; ok {
		t.Error("VITE_APP_NAME should not be injected")
	}
}

func TestInjectClientAPIEnv_UserOverridePreserved(t *testing.T) {
	clientDir := t.TempDir()
	os.WriteFile(filepath.Join(clientDir, ".env.example"),
		[]byte("VITE_API_BASE_URL=http://example.com/api\n"), 0644)

	userVal := "http://custom:9999/api"
	procs := map[string]Process{
		"client": {Cmd: "npm run dev", Env: map[string]string{
			"VITE_API_BASE_URL": userVal,
		}},
	}
	m := &Merge{Client: 5173, Server: 3000, Port: 8080}

	injectClientAPIEnv(procs, "client", clientDir, m)

	if got := procs["client"].Env["VITE_API_BASE_URL"]; got != userVal {
		t.Errorf("user-set env var was overridden: got %q, want %q", got, userVal)
	}
}

func TestInjectClientAPIEnv_FallsBackToDotEnv(t *testing.T) {
	clientDir := t.TempDir()
	// No .env.example, only .env
	os.WriteFile(filepath.Join(clientDir, ".env"),
		[]byte("VITE_API_BASE_URL=http://192.168.1.45:3000/api\n"), 0644)

	procs := map[string]Process{
		"client": {Cmd: "npm run dev"},
	}
	m := &Merge{Client: 5173, Server: 3000, Port: 8080}

	injectClientAPIEnv(procs, "client", clientDir, m)

	got := procs["client"].Env["VITE_API_BASE_URL"]
	want := "http://localhost:8080/api"
	if got != want {
		t.Errorf("VITE_API_BASE_URL = %q, want %q", got, want)
	}
}

// ─── Load integration: auto-scan + auto-inject ────────────────────────────────

func TestLoad_AutoScanAndInject(t *testing.T) {
	dir := t.TempDir()

	// Server source with detectable routes.
	serverDir := filepath.Join(dir, "server")
	os.MkdirAll(serverDir, 0755)
	os.WriteFile(filepath.Join(serverDir, "app.js"), []byte(`
app.use('/api', apiRouter)
app.get('/health', h)
`), 0644)

	// Client with .env.example for auto-injection.
	clientDir := filepath.Join(dir, "client")
	os.MkdirAll(clientDir, 0755)
	os.WriteFile(filepath.Join(clientDir, ".env.example"),
		[]byte("VITE_API_BASE_URL=http://192.168.1.45:3000/api\n"), 0644)

	yaml := `
merge:
  client: 5173
  server: 3000
  port: 8080

processes:
  client:
    cmd: npm run dev
    dir: ./client
    env:
      PORT: "5173"
  server:
    cmd: npm run dev
    dir: ./server
    env:
      PORT: "3000"
`
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Auto-scan should have populated api_prefixes.
	mergeCmd := cfg.Processes["merge-port"].Cmd
	if !strings.Contains(mergeCmd, "--api-prefix /api") {
		t.Errorf("expected --api-prefix /api in merge-port cmd, got %q", mergeCmd)
	}
	if !strings.Contains(mergeCmd, "--api-prefix /health") {
		t.Errorf("expected --api-prefix /health in merge-port cmd, got %q", mergeCmd)
	}

	// Auto-inject should have set VITE_API_BASE_URL on client process.
	clientEnv := cfg.Processes["client"].Env
	got := clientEnv["VITE_API_BASE_URL"]
	want := "http://localhost:8080/api"
	if got != want {
		t.Errorf("VITE_API_BASE_URL = %q, want %q", got, want)
	}
}

func TestLoad_EnvFile(t *testing.T) {
	dir := t.TempDir()
	envContent := "DB_HOST=localhost\nDB_PORT=5432\nSECRET=fromfile\n"
	envPath := filepath.Join(dir, ".env.backend")
	os.WriteFile(envPath, []byte(envContent), 0644)

	yaml := `
processes:
  backend:
    cmd: node dist/index.js
    env_file: .env.backend
`
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	env := cfg.Processes["backend"].Env
	if env["DB_HOST"] != "localhost" {
		t.Errorf("DB_HOST = %q, want %q", env["DB_HOST"], "localhost")
	}
	if env["DB_PORT"] != "5432" {
		t.Errorf("DB_PORT = %q, want %q", env["DB_PORT"], "5432")
	}
	if env["SECRET"] != "fromfile" {
		t.Errorf("SECRET = %q, want %q", env["SECRET"], "fromfile")
	}
}

func TestLoad_EnvFile_EnvBlockOverrides(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("PORT=3000\nSECRET=fromfile\n"), 0644)

	yaml := `
processes:
  backend:
    cmd: node dist/index.js
    env_file: .env
    env:
      PORT: "4000"
`
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	env := cfg.Processes["backend"].Env
	// env block wins over env_file
	if env["PORT"] != "4000" {
		t.Errorf("PORT = %q, want %q (env block should override env_file)", env["PORT"], "4000")
	}
	// env_file value not overridden by env block
	if env["SECRET"] != "fromfile" {
		t.Errorf("SECRET = %q, want %q", env["SECRET"], "fromfile")
	}
}

func TestBuildMergeCmd_PortEnvVar(t *testing.T) {
	tests := []struct {
		name    string
		merge   Merge
		portEnv string
		want    string
	}{
		{
			name:    "PORT env var used when merge.port is zero",
			merge:   Merge{Client: 3000, Server: 3001},
			portEnv: "3000",
			want:    "merge-port --client 3000 --server 3001 --port 3000",
		},
		{
			name:    "explicit merge.port overrides PORT env var",
			merge:   Merge{Client: 3000, Server: 3001, Port: 9000},
			portEnv: "5000",
			want:    "merge-port --client 3000 --server 3001 --port 9000",
		},
		{
			name:    "invalid PORT env var falls back to 8080",
			merge:   Merge{Client: 3000, Server: 3001},
			portEnv: "not-a-port",
			want:    "merge-port --client 3000 --server 3001 --port 8080",
		},
		{
			name:    "empty PORT env var falls back to 8080",
			merge:   Merge{Client: 3000, Server: 3001},
			portEnv: "",
			want:    "merge-port --client 3000 --server 3001 --port 8080",
		},
		{
			name:    "PORT env var used in route mode",
			merge:   Merge{Routes: []string{"/api=3001", "/=3000"}},
			portEnv: "5000",
			want:    "merge-port --route /api=3001 --route /=3000 --port 5000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT", tt.portEnv)
			got, err := buildMergeCmd(&tt.merge)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ─── Circular dependency detection ───────────────────────────────────────────

func TestLoad_CircularDependency_Direct(t *testing.T) {
	yaml := `
processes:
  a:
    cmd: echo a
    depends_on: [b]
  b:
    cmd: echo b
    depends_on: [a]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for circular dependency, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("expected circular dependency error, got: %v", err)
	}
}

func TestLoad_CircularDependency_Transitive(t *testing.T) {
	yaml := `
processes:
  a:
    cmd: echo a
    depends_on: [b]
  b:
    cmd: echo b
    depends_on: [c]
  c:
    cmd: echo c
    depends_on: [a]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for circular dependency, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("expected circular dependency error, got: %v", err)
	}
}

func TestLoad_RejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring expected in error
	}{
		{
			name: "typo restarts vs restart",
			yaml: `
processes:
  app:
    cmd: echo hi
    restarts: on-failure
`,
			want: "restarts",
		},
		{
			name: "typo dependsOn vs depends_on",
			yaml: `
processes:
  app:
    cmd: echo hi
  other:
    cmd: echo hi
    dependsOn: [app]
`,
			want: "dependsOn",
		},
		{
			name: "unknown top-level key",
			yaml: `
servicez:
  app:
    cmd: echo hi
`,
			want: "servicez",
		},
		{
			name: "unknown ready_when key",
			yaml: `
processes:
  app:
    cmd: echo hi
    ready_when:
      url: http://localhost:3000
`,
			want: "url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "proc-compose.yml")
			os.WriteFile(path, []byte(tc.yaml), 0644)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for unknown field, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestLoad_NoCycle_DAG(t *testing.T) {
	yaml := `
processes:
  a:
    cmd: echo a
    depends_on: [b, c]
  b:
    cmd: echo b
    depends_on: [c]
  c:
    cmd: echo c
`
	cfg := loadFromString(t, yaml)
	if len(cfg.Processes) != 3 {
		t.Fatalf("expected 3 processes, got %d", len(cfg.Processes))
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
