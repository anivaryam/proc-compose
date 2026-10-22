package config

import (
	"errors"
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

func TestLoad_DirRelativeToConfigFile(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}

	yaml := `
processes:
  client:
    cmd: pwd
    dir: ../client
`
	path := filepath.Join(configDir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	want := filepath.Clean(filepath.Join(configDir, "..", "client"))
	if got := cfg.Processes["client"].Dir; got != want {
		t.Fatalf("client dir = %q, want config-relative absolute path %q", got, want)
	}
}

func TestLoad_DirPreservesAbsoluteAndEmpty(t *testing.T) {
	dir := t.TempDir()
	absDir := filepath.Join(dir, "already-absolute")

	yaml := `
processes:
  absolute:
    cmd: pwd
    dir: ` + absDir + `
  empty:
    cmd: pwd
`
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if got := cfg.Processes["absolute"].Dir; got != filepath.Clean(absDir) {
		t.Fatalf("absolute dir = %q, want %q", got, filepath.Clean(absDir))
	}
	if got := cfg.Processes["empty"].Dir; got != "" {
		t.Fatalf("empty dir = %q, want empty", got)
	}
}

func TestLoad_DirLeavesInjectedMergePortEmpty(t *testing.T) {
	yaml := `
merge:
  client: 5173
  server: 3001

processes:
  app:
    cmd: npm start
`
	cfg := loadFromString(t, yaml)

	if got := cfg.Processes["merge-port"].Dir; got != "" {
		t.Fatalf("merge-port dir = %q, want empty", got)
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

// ─── Process mode validation ───────────────────────────────────────────────────

func TestLoad_ModeDefault(t *testing.T) {
	// No mode specified → defaults to "service"
	yaml := `
processes:
  app:
    cmd: echo hello
`
	cfg := loadFromString(t, yaml)
	if cfg.Processes["app"].Mode != "" {
		t.Errorf("Mode = %q, want empty string (raw)", cfg.Processes["app"].Mode)
	}
	if cfg.Processes["app"].EffectiveMode() != "service" {
		t.Errorf("EffectiveMode() = %q, want service", cfg.Processes["app"].EffectiveMode())
	}
}

func TestLoad_ModeService(t *testing.T) {
	yaml := `
processes:
  app:
    cmd: echo hello
    mode: service
`
	cfg := loadFromString(t, yaml)
	if cfg.Processes["app"].Mode != "service" {
		t.Errorf("Mode = %q, want service", cfg.Processes["app"].Mode)
	}
	if cfg.Processes["app"].EffectiveMode() != "service" {
		t.Errorf("EffectiveMode() = %q, want service", cfg.Processes["app"].EffectiveMode())
	}
}

func TestLoad_ModeTask(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
`
	cfg := loadFromString(t, yaml)
	if cfg.Processes["migrate"].Mode != "task" {
		t.Errorf("Mode = %q, want task", cfg.Processes["migrate"].Mode)
	}
	if cfg.Processes["migrate"].EffectiveMode() != "task" {
		t.Errorf("EffectiveMode() = %q, want task", cfg.Processes["migrate"].EffectiveMode())
	}
}

func TestLoad_ModeInvalid(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring expected in error
	}{
		{
			name: "unknown mode",
			yaml: `
processes:
  app:
    cmd: echo hi
    mode: once
`,
			want: `app"`,
		},
		{
			name: "typo task vs tasks",
			yaml: `
processes:
  app:
    cmd: echo hi
    mode: tasks
`,
			want: `app"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "proc-compose.yml")
			os.WriteFile(path, []byte(tc.yaml), 0644)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for invalid mode, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to mention %q, got: %v", tc.want, err)
			}
			// Error message should mention the invalid mode value
			if !strings.Contains(err.Error(), "mode") {
				t.Fatalf("expected error to mention 'mode', got: %v", err)
			}
		})
	}
}

// TestLoad_ReadyWhenCmd pins the cmd probe kind: a service accepts it, it
// counts toward ready_when's exactly-one rule, and a task still refuses it.
func TestLoad_ReadyWhenCmd(t *testing.T) {
	cfg := loadFromString(t, "processes:\n  worker:\n    cmd: run-worker\n    ready_when:\n      cmd: check-worker --ready\n")
	if rw := cfg.Processes["worker"].ReadyWhen; rw == nil || rw.Cmd != "check-worker --ready" {
		t.Fatalf("ready_when = %+v, want cmd probe", rw)
	}

	for name, probe := range map[string]string{
		"cmd and http": "      cmd: check\n      http: http://localhost:1/\n",
		"cmd and tcp":  "      cmd: check\n      tcp: localhost:1\n",
		"cmd and log":  "      cmd: check\n      log: ready\n",
		"empty cmd":    "      cmd: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "processes:\n  worker:\n    cmd: run-worker\n    ready_when:\n"+probe))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidReadyWhen || FormatPath(verr.Path) != "processes.worker.ready_when" {
				t.Fatalf("err = %v, want %s at processes.worker.ready_when", err, CodeInvalidReadyWhen)
			}
			if !strings.Contains(err.Error(), "cmd") {
				t.Fatalf("message does not name the cmd kind: %v", err)
			}
		})
	}

	_, err := Load(writeConfig(t, "processes:\n  migrate:\n    cmd: migrate\n    mode: task\n    ready_when:\n      cmd: check\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeTaskIncompatible {
		t.Fatalf("task with ready_when.cmd: err = %v, want %s", err, CodeTaskIncompatible)
	}
}

// TestLoad_HealthWhen pins the ongoing health probe fields: http, tcp and cmd
// are accepted with their defaults, and every misuse is refused with a stable
// code at the offending field.
func TestLoad_HealthWhen(t *testing.T) {
	for kind, probe := range map[string]string{
		"http": "      http: http://localhost:3000/health\n",
		"tcp":  "      tcp: localhost:5432\n",
		"cmd":  "      cmd: check-worker --live\n",
	} {
		t.Run("accepts "+kind, func(t *testing.T) {
			cfg := loadFromString(t, "processes:\n  worker:\n    cmd: run-worker\n    health_when:\n"+probe)
			proc := cfg.Processes["worker"]
			if proc.HealthWhen == nil || proc.EffectiveHealthInterval() != 10 || proc.EffectiveHealthAction() != HealthActionReport {
				t.Fatalf("health = %+v interval=%d action=%q, want probe with defaults 10/report", proc.HealthWhen, proc.EffectiveHealthInterval(), proc.EffectiveHealthAction())
			}
		})
	}
	cfg := loadFromString(t, "processes:\n  worker:\n    cmd: run-worker\n    restart: on-failure\n    max_restarts: 3\n    health_when:\n      tcp: localhost:1\n    health_interval: 2\n    health_action: restart\n")
	if proc := cfg.Processes["worker"]; proc.EffectiveHealthInterval() != 2 || proc.EffectiveHealthAction() != HealthActionRestart {
		t.Fatalf("explicit health fields = %+v", proc)
	}

	tests := []struct {
		name, body, code, path string
	}{
		{"log rejected", "    health_when:\n      log: ready\n", CodeInvalidReadyWhen, "processes.worker.health_when.log"},
		{"two kinds", "    health_when:\n      tcp: localhost:1\n      cmd: check\n", CodeInvalidReadyWhen, "processes.worker.health_when"},
		{"empty probe", "    health_when: {}\n", CodeInvalidReadyWhen, "processes.worker.health_when"},
		{"task health_when", "    mode: task\n    health_when:\n      tcp: localhost:1\n", CodeTaskIncompatible, "processes.worker.health_when"},
		{"task health_interval", "    mode: task\n    health_interval: 5\n", CodeTaskIncompatible, "processes.worker.health_interval"},
		{"task health_action", "    mode: task\n    health_action: report\n", CodeTaskIncompatible, "processes.worker.health_action"},
		{"orphan interval", "    health_interval: 5\n", CodeInvalidValue, "processes.worker.health_interval"},
		{"orphan action", "    health_action: report\n", CodeInvalidValue, "processes.worker.health_action"},
		{"negative interval", "    health_when:\n      tcp: localhost:1\n    health_interval: -1\n", CodeInvalidValue, "processes.worker.health_interval"},
		{"unknown action", "    health_when:\n      tcp: localhost:1\n    health_action: page\n", CodeInvalidValue, "processes.worker.health_action"},
		{"restart action with restart never", "    health_when:\n      tcp: localhost:1\n    health_action: restart\n", CodeInvalidValue, "processes.worker.health_action"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "processes:\n  worker:\n    cmd: run-worker\n"+tc.body))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != tc.code || FormatPath(verr.Path) != tc.path {
				t.Fatalf("err = %v (%+v), want %s at %s", err, verr, tc.code, tc.path)
			}
		})
	}
}

// TestLoad_ForeignListener pins foreign_listener: allow by default, reject
// accepted on a process with an http or tcp probe for it to apply to, and
// every misuse refused with a stable code at the field.
func TestLoad_ForeignListener(t *testing.T) {
	cfg := loadFromString(t, "processes:\n  api:\n    cmd: serve\n    ready_when:\n      http: http://localhost:3000/\n")
	if got := cfg.Processes["api"].EffectiveForeignListener(); got != ForeignListenerAllow {
		t.Fatalf("default foreign_listener = %q, want %q", got, ForeignListenerAllow)
	}
	for name, body := range map[string]string{
		"http readiness": "    ready_when:\n      http: http://localhost:3000/\n",
		"tcp readiness":  "    ready_when:\n      tcp: localhost:3000\n",
		"tcp health":     "    ready_when:\n      log: listening\n    health_when:\n      tcp: localhost:3000\n",
	} {
		t.Run("accepts reject with "+name, func(t *testing.T) {
			cfg := loadFromString(t, "processes:\n  api:\n    cmd: serve\n"+body+"    foreign_listener: reject\n")
			if got := cfg.Processes["api"].EffectiveForeignListener(); got != ForeignListenerReject {
				t.Fatalf("foreign_listener = %q, want reject", got)
			}
		})
	}
	for _, tc := range []struct{ name, body, code string }{
		{"unknown policy", "    ready_when:\n      tcp: localhost:1\n    foreign_listener: warn\n", CodeInvalidValue},
		{"no probe", "    foreign_listener: reject\n", CodeInvalidValue},
		{"only cmd and log probes", "    ready_when:\n      cmd: check\n    health_when:\n      cmd: check\n    foreign_listener: reject\n", CodeInvalidValue},
		{"task", "    mode: task\n    foreign_listener: reject\n", CodeTaskIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: serve\n"+tc.body))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != tc.code || FormatPath(verr.Path) != "processes.api.foreign_listener" {
				t.Fatalf("err = %v (%+v), want %s at processes.api.foreign_listener", err, verr, tc.code)
			}
		})
	}
	// Enum parity with the schema: every published policy loads.
	for _, policy := range ForeignListenerPolicies {
		loadFromString(t, "processes:\n  api:\n    cmd: serve\n    ready_when:\n      tcp: localhost:1\n    foreign_listener: "+policy+"\n")
	}
}

func TestLoad_ModeTaskRejectsReadyWhen(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    ready_when:
      log: "migration complete"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for ready_when with mode: task, got nil")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention process name 'migrate', got: %v", err)
	}
	if !strings.Contains(err.Error(), "mode: task") {
		t.Fatalf("expected error to mention 'mode: task', got: %v", err)
	}
	if !strings.Contains(err.Error(), "ready_when") {
		t.Fatalf("expected error to mention 'ready_when', got: %v", err)
	}
}

func TestLoad_ModeTaskRejectsReadyTimeout(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    ready_timeout: 30
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for ready_timeout with mode: task, got nil")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention process name 'migrate', got: %v", err)
	}
	if !strings.Contains(err.Error(), "mode: task") {
		t.Fatalf("expected error to mention 'mode: task', got: %v", err)
	}
	if !strings.Contains(err.Error(), "ready_timeout") {
		t.Fatalf("expected error to mention 'ready_timeout', got: %v", err)
	}
}

func TestLoad_ModeTaskRejectsRestartOnFailure(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    restart: on-failure
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for restart: on-failure with mode: task, got nil")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention process name 'migrate', got: %v", err)
	}
	if !strings.Contains(err.Error(), "mode: task") {
		t.Fatalf("expected error to mention 'mode: task', got: %v", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("expected error to mention 'restart', got: %v", err)
	}
}

func TestLoad_ModeTaskRejectsRestartAlways(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    restart: always
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for restart: always with mode: task, got nil")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention process name 'migrate', got: %v", err)
	}
	if !strings.Contains(err.Error(), "mode: task") {
		t.Fatalf("expected error to mention 'mode: task', got: %v", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("expected error to mention 'restart', got: %v", err)
	}
}

func TestLoad_ModeTaskRejectsMaxRestarts(t *testing.T) {
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    max_restarts: 5
`
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for max_restarts with mode: task, got nil")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected error to mention process name 'migrate', got: %v", err)
	}
	if !strings.Contains(err.Error(), "mode: task") {
		t.Fatalf("expected error to mention 'mode: task', got: %v", err)
	}
	if !strings.Contains(err.Error(), "max_restarts") {
		t.Fatalf("expected error to mention 'max_restarts', got: %v", err)
	}
}

func TestLoad_ModeTaskAllowsRestartNever(t *testing.T) {
	// mode: task with restart: never (or no restart) should be valid
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
    restart: never
`
	cfg := loadFromString(t, yaml)
	if cfg.Processes["migrate"].Mode != "task" {
		t.Errorf("Mode = %q, want task", cfg.Processes["migrate"].Mode)
	}
	if cfg.Processes["migrate"].Restart != "never" {
		t.Errorf("Restart = %q, want never", cfg.Processes["migrate"].Restart)
	}
}

func TestLoad_ModeTaskAllowsNoReadyWhenOrTimeout(t *testing.T) {
	// mode: task with no ready_when and no ready_timeout should be valid
	yaml := `
processes:
  migrate:
    cmd: db-migrate
    mode: task
`
	cfg := loadFromString(t, yaml)
	if cfg.Processes["migrate"].Mode != "task" {
		t.Errorf("Mode = %q, want task", cfg.Processes["migrate"].Mode)
	}
	if cfg.Processes["migrate"].ReadyWhen != nil {
		t.Errorf("ReadyWhen = %v, want nil", cfg.Processes["migrate"].ReadyWhen)
	}
	if cfg.Processes["migrate"].ReadyTimeout != 0 {
		t.Errorf("ReadyTimeout = %d, want 0", cfg.Processes["migrate"].ReadyTimeout)
	}
}

// TestLoad_ModeTeardown pins mode: teardown. A teardown process takes only
// cmd, dir, env, env_file and a shutdown_timeout of at most 5s; every
// startup, restart or health field is refused at the offending field, nothing
// may depend on it, and it is never part of a startup selection.
func TestLoad_ModeTeardown(t *testing.T) {
	cfg := loadFromString(t, `processes:
  api:
    cmd: api
  cleanup:
    mode: teardown
    cmd: rm -rf ./uploads
    dir: .
    env:
      KEEP: "0"
    shutdown_timeout: 5
`)
	if got := cfg.Processes["cleanup"].EffectiveMode(); got != ProcessModeTeardown {
		t.Fatalf("EffectiveMode() = %q, want teardown", got)
	}
	if got := SelectedNames(cfg, nil); !equalSlices(got, []string{"api"}) {
		t.Fatalf("SelectedNames = %v, want only the service", got)
	}
	if got := TeardownNames(cfg); !equalSlices(got, []string{"cleanup"}) {
		t.Fatalf("TeardownNames = %v, want [cleanup]", got)
	}
	if err := CheckSelector(cfg, "cleanup"); err == nil || !strings.Contains(err.Error(), "teardown") {
		t.Fatalf("CheckSelector(cleanup) = %v, want a teardown refusal", err)
	}
	if err := CheckSelector(cfg, "nope"); err == nil || !strings.Contains(err.Error(), `unknown process "nope"`) {
		t.Fatalf("CheckSelector(nope) = %v, want unknown process", err)
	}
	if err := CheckSelector(cfg, "api"); err != nil {
		t.Fatalf("CheckSelector(api) = %v", err)
	}

	for _, tc := range []struct{ field, yaml, code string }{
		{"ready_when", "    ready_when:\n      log: done\n", CodeTaskIncompatible},
		{"ready_timeout", "    ready_timeout: 5\n", CodeTaskIncompatible},
		{"restart", "    restart: on-failure\n", CodeTaskIncompatible},
		{"health_when", "    health_when:\n      tcp: localhost:1\n", CodeTaskIncompatible},
		{"health_interval", "    health_interval: 3\n", CodeTaskIncompatible},
		{"health_action", "    health_action: report\n", CodeTaskIncompatible},
		{"foreign_listener", "    foreign_listener: reject\n", CodeTaskIncompatible},
		{"ports", "    ports:\n      http: 0\n", CodeTaskIncompatible},
		{"depends_on", "    depends_on: [api]\n", CodeTaskIncompatible},
		{"shutdown_timeout", "    shutdown_timeout: 6\n", CodeInvalidValue},
	} {
		t.Run(tc.field, func(t *testing.T) {
			_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: api\n  cleanup:\n    cmd: x\n    mode: teardown\n"+tc.yaml))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != tc.code || FormatPath(verr.Path) != "processes.cleanup."+tc.field {
				t.Fatalf("err = %v, want %s at processes.cleanup.%s", err, tc.code, tc.field)
			}
		})
	}

	_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: api\n    depends_on: [cleanup]\n  cleanup:\n    cmd: x\n    mode: teardown\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeTeardownDependency || FormatPath(verr.Path) != "processes.api.depends_on[0]" {
		t.Fatalf("depending on a teardown: err = %v, want %s at processes.api.depends_on[0]", err, CodeTeardownDependency)
	}
}

// TestSelectedNames_UnknownFilterSelectsNothing pins the canonical closure's
// treatment of a filter that names no configured process: the selection is
// empty, so an unknown name is never started as if it were a process. The
// teardown exclusion is pinned by TestLoad_ModeTeardown.
func TestSelectedNames_UnknownFilterSelectsNothing(t *testing.T) {
	cfg := loadFromString(t, `processes:
  api:
    cmd: api
`)
	if got := SelectedNames(cfg, []string{"nope"}); len(got) != 0 {
		t.Fatalf("SelectedNames = %v, want an empty selection", got)
	}
	if got := SelectedNames(cfg, []string{"api", "nope"}); !equalSlices(got, []string{"api"}) {
		t.Fatalf("SelectedNames = %v, want only the known process", got)
	}
}

// TestLoad_NormalisesResourceLabels pins that a resource label is normalised
// once, at load time: the trimmed label is the identity the lease, the
// duplicate check and Project.Resources all use. A label that differs only in
// surrounding whitespace names the same resource, so the first occurrence
// wins and no error is raised; an exactly repeated label is still refused.
func TestLoad_NormalisesResourceLabels(t *testing.T) {
	t.Run("whitespace variants name one resource", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [db, \" db \", \"  db  \"]\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"db"}; !equalSlices(cfg.Resources, want) {
			t.Fatalf("resources = %q, want %q", cfg.Resources, want)
		}
	})
	t.Run("the trimmed label is kept", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\" db \", cache]\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"db", "cache"}; !equalSlices(cfg.Resources, want) {
			t.Fatalf("resources = %q, want %q", cfg.Resources, want)
		}
	})
	t.Run("an exactly repeated label is still refused", func(t *testing.T) {
		_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [db, db]\n"))
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Code != CodeInvalidResource || FormatPath(verr.Path) != "resources[1]" {
			t.Fatalf("err = %v, want %s at resources[1]", err, CodeInvalidResource)
		}
	})
	t.Run("a whitespace-only label is still refused", func(t *testing.T) {
		_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\"  \"]\n"))
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Code != CodeInvalidResource || FormatPath(verr.Path) != "resources[0]" {
			t.Fatalf("err = %v, want %s at resources[0]", err, CodeInvalidResource)
		}
	})
	t.Run("the length limit counts the trimmed label", func(t *testing.T) {
		long := strings.Repeat("a", 257)
		_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\""+long+"\"]\n"))
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Code != CodeInvalidResource || FormatPath(verr.Path) != "resources[0]" {
			t.Fatalf("a 257-character label: err = %v, want %s at resources[0]", err, CodeInvalidResource)
		}
		// The same label padded with whitespace is 257 bytes raw but 256
		// characters trimmed: the limit counts the trimmed label, so it is
		// accepted and normalised.
		cfg, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\""+strings.Repeat("a", 256)+" \"]\n"))
		if err != nil {
			t.Fatalf("a padded 257-byte label: %v", err)
		}
		if want := []string{strings.Repeat("a", 256)}; !equalSlices(cfg.Resources, want) {
			t.Fatalf("resources = %q, want one 256-character label", cfg.Resources)
		}
		cfg, err = Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\""+strings.Repeat("a", 256)+"\"]\n"))
		if err != nil {
			t.Fatalf("a 256-character label: %v", err)
		}
		if want := []string{strings.Repeat("a", 256)}; !equalSlices(cfg.Resources, want) {
			t.Fatalf("resources = %q, want one 256-character label", cfg.Resources)
		}
	})
	t.Run("the character check runs on the trimmed label", func(t *testing.T) {
		// An inner carriage return is part of the label: refused.
		_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\"a\\rb\"]\n"))
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Code != CodeInvalidResource || FormatPath(verr.Path) != "resources[0]" {
			t.Fatalf("an inner carriage return: err = %v, want %s at resources[0]", err, CodeInvalidResource)
		}
		// A trailing one is surrounding whitespace: trimmed away, so the
		// label is the same resource as "db".
		cfg, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\nresources: [\"db\\r\"]\n"))
		if err != nil {
			t.Fatalf("a trailing carriage return: %v", err)
		}
		if want := []string{"db"}; !equalSlices(cfg.Resources, want) {
			t.Fatalf("resources = %q, want %q", cfg.Resources, want)
		}
	})
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

// ─── #18: a merge upstream must be a service ──────────────────────────────────

// TestLoad_MergeUpstreamMustBeService pins that only a service can be a merge
// upstream. A task or teardown process is never selected, so the merge wiring
// writes neither a readiness probe into it nor a dependency edge to it.
func TestLoad_MergeUpstreamMustBeService(t *testing.T) {
	t.Run("a task matched by PORT is not an upstream", func(t *testing.T) {
		t.Setenv("PORT", "")
		cfg := loadFromString(t, `merge:
  client: 5173
  server: 3001

processes:
  api:
    mode: task
    cmd: seed
    env:
      PORT: "3001"
`)
		if rw := cfg.Processes["api"].ReadyWhen; rw != nil {
			t.Fatalf("api ready_when = %+v, want none", rw)
		}
		if contains(cfg.Processes["merge-port"].DependsOn, "api") {
			t.Fatalf("merge-port depends_on = %v, want no task upstream", cfg.Processes["merge-port"].DependsOn)
		}
	})
	t.Run("a task matched by name is not an upstream", func(t *testing.T) {
		t.Setenv("PORT", "")
		cfg := loadFromString(t, `merge:
  client: 3000
  server: 3001

processes:
  web:
    mode: task
    cmd: build
`)
		if rw := cfg.Processes["web"].ReadyWhen; rw != nil {
			t.Fatalf("web ready_when = %+v, want none", rw)
		}
		if contains(cfg.Processes["merge-port"].DependsOn, "web") {
			t.Fatalf("merge-port depends_on = %v, want no task upstream", cfg.Processes["merge-port"].DependsOn)
		}
	})
	t.Run("a teardown process is not an upstream", func(t *testing.T) {
		t.Setenv("PORT", "")
		cfg := loadFromString(t, `merge:
  client: 5173
  server: 3001

processes:
  server:
    mode: teardown
    cmd: cleanup
    env:
      PORT: "3001"
`)
		if rw := cfg.Processes["server"].ReadyWhen; rw != nil {
			t.Fatalf("server ready_when = %+v, want none", rw)
		}
		if contains(cfg.Processes["merge-port"].DependsOn, "server") {
			t.Fatalf("merge-port depends_on = %v, want no teardown upstream", cfg.Processes["merge-port"].DependsOn)
		}
	})
}

// TestLoad_MergeClientProcessMustBeAService pins that merge.client_process
// cannot name a task or teardown process: the upstream wiring skips a
// non-service, so the client env injection must not target it either. The
// config is refused at merge.client_process instead.
func TestLoad_MergeClientProcessMustBeAService(t *testing.T) {
	t.Setenv("PORT", "")
	for _, mode := range []string{ProcessModeTask, ProcessModeTeardown} {
		t.Run(mode, func(t *testing.T) {
			_, err := Load(writeConfig(t, "merge:\n  client: 3000\n  server: 3001\n  client_process: build\n\nprocesses:\n  build:\n    cmd: npm run build\n    mode: "+mode+"\n"))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge || FormatPath(verr.Path) != "merge.client_process" {
				t.Fatalf("err = %v, want %s at merge.client_process", err, CodeInvalidMerge)
			}
			if !strings.Contains(verr.Msg, "only a service can be the merge client") {
				t.Fatalf("message = %q, want the service rule", verr.Msg)
			}
		})
	}
	// A service named by client_process is still the client.
	cfg := loadFromString(t, "merge:\n  client: 3000\n  server: 3001\n  client_process: site\n\nprocesses:\n  site:\n    cmd: npm start\n")
	if rw := cfg.Processes["site"].ReadyWhen; rw == nil || rw.TCP != "localhost:3000" {
		t.Fatalf("site ready_when = %+v, want localhost:3000", rw)
	}
}

// TestLoad_MergeSelectionsSkipNonServices pins that the api_prefixes scan and
// the client env injection pick from the same service candidates as the
// upstream wiring: a task named like the server is not scanned, and a task
// named like the client gets no injected API URL.
func TestLoad_MergeSelectionsSkipNonServices(t *testing.T) {
	t.Setenv("PORT", "")
	dir := t.TempDir()
	for name, content := range map[string]string{
		"server/app.js":    "app.use('/tasks', h)\n",
		"web/.env.example": "VITE_API_URL=http://localhost:3001/api\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "proc-compose.yml")
	yaml := "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  server:\n    cmd: seed\n    mode: task\n    dir: ./server\n  web:\n    cmd: npm run build\n    mode: task\n    dir: ./web\n"
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cmd := cfg.Processes[MergePortProcessName].Cmd; strings.Contains(cmd, "--api-prefix") {
		t.Fatalf("merge-port cmd = %q, want no prefix scanned from a task", cmd)
	}
	if got, ok := cfg.Processes["web"].Env["VITE_API_URL"]; ok {
		t.Fatalf("web VITE_API_URL = %q, want no injection into a task", got)
	}
}

// ─── #19: the generated proxy's name is reserved ──────────────────────────────

// TestLoad_RejectsReservedMergePortProcessName pins that a user process named
// like the generated proxy is refused instead of being overwritten.
func TestLoad_RejectsReservedMergePortProcessName(t *testing.T) {
	t.Setenv("PORT", "")
	_, err := Load(writeConfig(t, "merge:\n  client: 5173\n  server: 3001\nprocesses:\n  merge-port:\n    cmd: my-own-proxy --serve\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeReservedProcessName || FormatPath(verr.Path) != "processes.merge-port" {
		t.Fatalf("err = %v, want %s at processes.merge-port", err, CodeReservedProcessName)
	}
	// Without a merge section no proxy is generated, so the name is free.
	cfg, err := Load(writeConfig(t, "processes:\n  merge-port:\n    cmd: my-own-proxy --serve\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Processes["merge-port"].Cmd; got != "my-own-proxy --serve" {
		t.Fatalf("merge-port cmd = %q, want the user's own command", got)
	}
}

// ─── #20: the merge upstream selection is deterministic ───────────────────────

// TestLoad_MergeUpstreamSelectionIsDeterministic pins that a duplicated PORT
// always resolves to the same upstream: the candidates are visited in name
// order, so the probe, the dependency edge and the proxy command line are the
// same on every load.
func TestLoad_MergeUpstreamSelectionIsDeterministic(t *testing.T) {
	t.Setenv("PORT", "")
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "app.js"), []byte("app.use('/"+name+"', h)\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	yaml := `merge:
  client: 3000
  server: 3001

processes:
  alpha:
    cmd: npm run dev
    dir: ./alpha
    env:
      PORT: "3001"
  beta:
    cmd: npm run dev
    dir: ./beta
    env:
      PORT: "3001"
`
	path := filepath.Join(dir, "proc-compose.yml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	var first *Config
	for i := 0; i < 50; i++ {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if first == nil {
			first = cfg
			continue
		}
		if got, want := cfg.Processes["merge-port"].Cmd, first.Processes["merge-port"].Cmd; got != want {
			t.Fatalf("load %d: merge-port cmd = %q, want %q", i, got, want)
		}
		if got, want := cfg.Processes["merge-port"].DependsOn, first.Processes["merge-port"].DependsOn; !equalSlices(got, want) {
			t.Fatalf("load %d: merge-port depends_on = %v, want %v", i, got, want)
		}
	}
	// The lexicographically first candidate wins, and the route scan follows
	// the same selection.
	if want := []string{"alpha"}; !equalSlices(first.Processes["merge-port"].DependsOn, want) {
		t.Fatalf("merge-port depends_on = %v, want %v", first.Processes["merge-port"].DependsOn, want)
	}
	if rw := first.Processes["alpha"].ReadyWhen; rw == nil || rw.TCP != "localhost:3001" {
		t.Fatalf("alpha ready_when = %v, want localhost:3001", rw)
	}
	if rw := first.Processes["beta"].ReadyWhen; rw != nil {
		t.Fatalf("beta ready_when = %+v, want none", rw)
	}
	if cmd := first.Processes["merge-port"].Cmd; !strings.Contains(cmd, "--api-prefix /alpha") {
		t.Fatalf("merge-port cmd = %q, want the alpha route prefix", cmd)
	}
}

// ─── #21: an upstream must serve the merge port ───────────────────────────────

// TestLoad_RefusesUpstreamWithADifferentPort pins that a merge port is never
// probed on a port the selected process does not serve: the config is refused
// with a located diagnostic on all three selection paths.
func TestLoad_RefusesUpstreamWithADifferentPort(t *testing.T) {
	t.Setenv("PORT", "")
	cases := []struct {
		name    string
		yaml    string
		envFile string // written next to the config as .env
		path    string
		message string
	}{
		{
			name: "client name fallback",
			yaml: "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  web:\n    cmd: npm start\n    env:\n      PORT: \"5173\"\n",
			path: "merge.client",
		},
		{
			name: "server name fallback",
			yaml: "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  server:\n    cmd: go run .\n    env:\n      PORT: \"4000\"\n",
			path: "merge.server",
		},
		{
			name: "explicit client_process override",
			yaml: "merge:\n  client: 3000\n  server: 3001\n  client_process: webapp\n\nprocesses:\n  webapp:\n    cmd: npm start\n    env:\n      PORT: \"5173\"\n",
			path: "merge.client",
		},
		// The PORT a process is started with may come from its env_file, from
		// its ports.http port, or from a resolvable ${ports.*} reference.
		{
			name:    "a PORT declared in the env_file",
			yaml:    "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  web:\n    cmd: npm start\n    env_file: .env\n",
			envFile: "PORT=5173\n",
			path:    "merge.client",
			message: `declares PORT "5173" in its env_file but merge.client is 3000`,
		},
		{
			name:    "a PORT given by ports.http",
			yaml:    "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  web:\n    cmd: npm start\n    ports:\n      http: 5173\n",
			path:    "merge.client",
			message: "is given PORT 5173 by ports.http but merge.client is 3000",
		},
		{
			name:    "a PORT that references a named port",
			yaml:    "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  web:\n    cmd: npm start\n    ports:\n      http: 5173\n    env:\n      PORT: \"${ports.web.http}\"\n",
			path:    "merge.client",
			message: `declares PORT "${ports.web.http}" (5173) but merge.client is 3000`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			if tc.envFile != "" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte(tc.envFile), 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(path)
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge || FormatPath(verr.Path) != tc.path {
				t.Fatalf("err = %v, want %s at %s", err, CodeInvalidMerge, tc.path)
			}
			if !strings.Contains(verr.Msg, tc.message) {
				t.Fatalf("message = %q, want it to contain %q", verr.Msg, tc.message)
			}
		})
	}
}

// TestLoad_TemplatedUpstreamPortIsResolved pins the #21 regression: a PORT
// written as a ${ports.*} reference is resolved to the referenced preferred
// port before it is compared, never compared as a string. This config loaded
// before the upstream check existed and must keep loading.
func TestLoad_TemplatedUpstreamPortIsResolved(t *testing.T) {
	t.Setenv("PORT", "")
	cfg := loadFromString(t, `processes:
  web:
    cmd: node server.js
    ports:
      http: 3000
    env:
      PORT: "${ports.web.http}"
  api:
    cmd: node api.js
    env:
      PORT: "4000"
merge:
  client: 3000
  server: 4000
`)
	if rw := cfg.Processes["web"].ReadyWhen; rw == nil || rw.TCP != "localhost:3000" {
		t.Fatalf("web ready_when = %+v, want localhost:3000", rw)
	}
	if rw := cfg.Processes["api"].ReadyWhen; rw == nil || rw.TCP != "localhost:4000" {
		t.Fatalf("api ready_when = %+v, want localhost:4000", rw)
	}
	if got, want := cfg.Processes[MergePortProcessName].DependsOn, []string{"api", "web"}; !equalSlices(got, want) {
		t.Fatalf("merge-port depends_on = %v, want %v", got, want)
	}
	// The reference itself is left for the session to expand.
	if got := cfg.Processes["web"].Env["PORT"]; got != "${ports.web.http}" {
		t.Fatalf("web PORT = %q, want the reference as written", got)
	}
}

// TestLoad_UpstreamMatchedByItsServedPort pins that a process is matched to a
// merge port by the PORT it is started with, wherever that PORT is declared:
// a process that is not named like a client is still found by its env_file
// PORT, its ports.http port or a resolved reference.
func TestLoad_UpstreamMatchedByItsServedPort(t *testing.T) {
	t.Setenv("PORT", "")
	cases := []struct {
		name, process, envFile string
	}{
		{"env_file PORT", "    env_file: .env\n", "PORT=3000\n"},
		{"ports.http", "    ports:\n      http: 3000\n", ""},
		{"a reference to another process's port", "    env:\n      PORT: \"${ports.other.http}\"\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  site:\n    cmd: npm start\n"+tc.process+
				"  other:\n    cmd: sleep 60\n    mode: task\n    ports:\n      http: 3000\n")
			if tc.envFile != "" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte(tc.envFile), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if rw := cfg.Processes["site"].ReadyWhen; rw == nil || rw.TCP != "localhost:3000" {
				t.Fatalf("site ready_when = %+v, want localhost:3000", rw)
			}
			if !contains(cfg.Processes[MergePortProcessName].DependsOn, "site") {
				t.Fatalf("merge-port depends_on = %v, want site", cfg.Processes[MergePortProcessName].DependsOn)
			}
		})
	}
}

// TestLoad_UnknowableUpstreamPortIsProbedAtTheMergePort pins the documented
// guess for a PORT Load cannot know: an ephemeral ports.http port, a
// reference to it, or a reference the session alone resolves. The process is
// probed at the merge port instead of being refused.
func TestLoad_UnknowableUpstreamPortIsProbedAtTheMergePort(t *testing.T) {
	t.Setenv("PORT", "")
	for name, process := range map[string]string{
		"an ephemeral ports.http":          "    ports:\n      http: 0\n",
		"a reference to an ephemeral port": "    ports:\n      http: 0\n    env:\n      PORT: \"${ports.web.http}\"\n",
		"a session reference":              "    env:\n      PORT: \"${session.id}\"\n",
		"an undeclared port reference":     "    env:\n      PORT: \"${ports.web.http}\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := loadFromString(t, "merge:\n  client: 3000\n  server: 3001\n\nprocesses:\n  web:\n    cmd: npm start\n"+process)
			if rw := cfg.Processes["web"].ReadyWhen; rw == nil || rw.TCP != "localhost:3000" {
				t.Fatalf("web ready_when = %+v, want localhost:3000", rw)
			}
		})
	}
}

// TestLoad_NameMatchedUpstreamWithoutAPortIsProbedAtTheMergePort pins the
// documented guess: a candidate that declares no PORT is still probed at the
// merge port and still gets the dependency edge.
func TestLoad_NameMatchedUpstreamWithoutAPortIsProbedAtTheMergePort(t *testing.T) {
	t.Setenv("PORT", "")
	cfg := loadFromString(t, `merge:
  client: 3000
  server: 3001

processes:
  web:
    cmd: npm start
`)
	rw := cfg.Processes["web"].ReadyWhen
	if rw == nil || rw.TCP != "localhost:3000" {
		t.Fatalf("web ready_when = %v, want localhost:3000", rw)
	}
	if !contains(cfg.Processes["merge-port"].DependsOn, "web") {
		t.Fatalf("merge-port depends_on = %v, want web", cfg.Processes["merge-port"].DependsOn)
	}
}

// TestLoad_UpstreamPortIsComparedNumerically pins that the declared PORT is
// compared as a number: PORT: "05173" is the merge port 5173, not a mismatch.
func TestLoad_UpstreamPortIsComparedNumerically(t *testing.T) {
	t.Setenv("PORT", "")
	cfg := loadFromString(t, `merge:
  client: 5173
  server: 3001

processes:
  client:
    cmd: npm start
    env:
      PORT: "05173"
`)
	rw := cfg.Processes["client"].ReadyWhen
	if rw == nil || rw.TCP != "localhost:5173" {
		t.Fatalf("client ready_when = %v, want localhost:5173", rw)
	}
}

// ─── #22: an unreadable env file is an error, not a truncation ───────────────

// TestParseEnvFile_LargeValue pins that a value larger than the old 64 KiB
// token limit is parsed in full.
func TestParseEnvFile_LargeValue(t *testing.T) {
	dir := t.TempDir()
	value := strings.Repeat("x", 70000)
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BIG="+value+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile: %v", err)
	}
	if len(got["BIG"]) != len(value) {
		t.Fatalf("BIG has %d bytes, want %d (truncated)", len(got["BIG"]), len(value))
	}
}

// TestLoad_EnvFileRejectsAnOversizedLine pins that a line longer than 1 MiB is
// a located error naming the file, instead of a silently truncated
// environment.
func TestLoad_EnvFileRejectsAnOversizedLine(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("BIG="+strings.Repeat("x", 1024*1024)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "proc-compose.yml")
	if err := os.WriteFile(path, []byte("processes:\n  api:\n    cmd: x\n    env_file: .env\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeEnvFile {
		t.Fatalf("err = %v, want %s", err, CodeEnvFile)
	}
	if !strings.Contains(err.Error(), envPath) {
		t.Fatalf("error %q does not name the env file %q", err, envPath)
	}
}

// TestParseEnvFile_LineLengthBoundary pins the documented limit: a line of up
// to 1 MiB (1,048,576 bytes, not counting its line terminator) is parsed in
// full whatever the line ending, and one byte more is an error that names the
// line.
func TestParseEnvFile_LineLengthBoundary(t *testing.T) {
	const limit = 1 << 20
	line := func(n int) string { return "BIG=" + strings.Repeat("x", n-len("BIG=")) }
	for _, ending := range []struct{ name, terminator string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"no newline at EOF", ""}} {
		t.Run(ending.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			for _, n := range []int{limit - 1, limit} {
				if err := os.WriteFile(path, []byte("A=1\n"+line(n)+ending.terminator), 0644); err != nil {
					t.Fatal(err)
				}
				got, err := parseEnvFile(path)
				if err != nil {
					t.Fatalf("a %d-byte line: %v", n, err)
				}
				if len(got["BIG"]) != n-len("BIG=") || got["A"] != "1" {
					t.Fatalf("a %d-byte line: BIG has %d bytes, A = %q", n, len(got["BIG"]), got["A"])
				}
			}
			if err := os.WriteFile(path, []byte("A=1\n"+line(limit+1)+ending.terminator+"C=3\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := parseEnvFile(path); err == nil || !strings.Contains(err.Error(), "line 2 is longer than 1 MiB") {
				t.Fatalf("a %d-byte line: err = %v, want a line 2 length error", limit+1, err)
			}
		})
	}
}

// ─── #49: port names carry no dot ────────────────────────────────────────────

// TestLoad_RejectsDottedPortNames pins that a dotted port name is refused:
// the endpoint reference name is "process.port", so a dot in the port name
// would let two endpoints claim one reference.
func TestLoad_RejectsDottedPortNames(t *testing.T) {
	_, err := Load(writeConfig(t, "processes:\n  a:\n    cmd: x\n    ports:\n      b.c: 0\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeInvalidPort || FormatPath(verr.Path) != "processes.a.ports.b.c" {
		t.Fatalf("err = %v, want %s at processes.a.ports.b.c", err, CodeInvalidPort)
	}
	// The message names the rule that was broken.
	if !strings.Contains(verr.Msg, "`.` is not allowed in port names") {
		t.Fatalf("message = %q, want the dot rule", verr.Msg)
	}
}

// TestLoad_RejectsTheCollidingPortConfig pins that the configuration the
// collision net guards against no longer loads: the dotted port name is
// refused by the port-name rule.
func TestLoad_RejectsTheCollidingPortConfig(t *testing.T) {
	_, err := Load(writeConfig(t, "processes:\n  a:\n    cmd: x\n    ports:\n      b.c: 0\n  a.b:\n    cmd: y\n    ports:\n      c: 0\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeInvalidPort {
		t.Fatalf("err = %v, want %s", err, CodeInvalidPort)
	}
}

// TestCheckReferenceCollisions_ReportsBothNamespaces pins the safety net
// itself: it names the colliding pair in the ports. and the urls. namespace.
func TestCheckReferenceCollisions_ReportsBothNamespaces(t *testing.T) {
	procs := map[string]Process{
		"a":   {Cmd: "x", Ports: map[string]int{"b.c": 0}},
		"a.b": {Cmd: "y", Ports: map[string]int{"c": 0}},
	}
	err := checkReferenceCollisions(procs)
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
	for _, want := range []string{"ports.a.b.c", "urls.a.b.c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error %q does not name %q", err, want)
		}
	}
}

// TestCheckReferenceCollisions_AcceptsDotFreeNames pins that the safety net
// stays quiet for the names the loader accepts.
func TestCheckReferenceCollisions_AcceptsDotFreeNames(t *testing.T) {
	procs := map[string]Process{
		"a":   {Cmd: "x", Ports: map[string]int{"b": 0}},
		"a.b": {Cmd: "y", Ports: map[string]int{"c": 0}},
	}
	if err := checkReferenceCollisions(procs); err != nil {
		t.Fatalf("checkReferenceCollisions: %v", err)
	}
}

// ─── #50: the merge ports are in range ────────────────────────────────────────

// TestBuildMergeCmd_RejectsOutOfRangePorts pins the 1-65535 range for the
// merge client, server and listen port.
func TestBuildMergeCmd_RejectsOutOfRangePorts(t *testing.T) {
	cases := []struct {
		name     string
		merge    Merge
		wantErr  string
		wantPath string
	}{
		{"client out of range", Merge{Client: 99999, Server: 3001}, "merge.client", "merge.client"},
		{"negative client", Merge{Client: -5, Server: 3001}, "merge.client", "merge.client"},
		{"server out of range", Merge{Client: 3000, Server: 70000}, "merge.server", "merge.server"},
		{"listen port out of range", Merge{Client: 3000, Server: 3001, Port: 70000}, "merge.port", "merge.port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PORT", "")
			_, err := buildMergeCmd(&tc.merge)
			if err == nil {
				t.Fatalf("expected an error for %v, got nil", tc.merge)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %q", err, tc.wantErr)
			}
			// The failure names the field it is about: the path is precise,
			// not the whole merge section.
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge {
				t.Fatalf("err = %v, want %s", err, CodeInvalidMerge)
			}
			if got := FormatPath(verr.Path); got != tc.wantPath {
				t.Fatalf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// TestLoad_MergeOutOfRangePortIsRefused pins that an out-of-range merge port
// is refused at load time with the invalid_merge code.
func TestLoad_MergeOutOfRangePortIsRefused(t *testing.T) {
	t.Setenv("PORT", "")
	_, err := Load(writeConfig(t, "merge:\n  client: 3000\n  server: 3001\n  port: 70000\nprocesses:\n  api:\n    cmd: x\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge {
		t.Fatalf("err = %v, want %s", err, CodeInvalidMerge)
	}
	// The diagnostic names the offending field, not the whole merge section.
	if got := FormatPath(verr.Path); got != "merge.port" {
		t.Fatalf("path = %q, want merge.port", got)
	}
}

// TestLoad_MergeOutOfRangeClientAndServerAreRefusedAtTheirField pins that the
// client and server ports are refused at their own field too.
func TestLoad_MergeOutOfRangeClientAndServerAreRefusedAtTheirField(t *testing.T) {
	t.Setenv("PORT", "")
	cases := []struct {
		name string
		yaml string
		path string
	}{
		{"client", "merge:\n  client: 99999\n  server: 3001\nprocesses:\n  api:\n    cmd: x\n", "merge.client"},
		{"server", "merge:\n  client: 3000\n  server: 70000\nprocesses:\n  api:\n    cmd: x\n", "merge.server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge {
				t.Fatalf("err = %v, want %s", err, CodeInvalidMerge)
			}
			if got := FormatPath(verr.Path); got != tc.path {
				t.Fatalf("path = %q, want %q", got, tc.path)
			}
		})
	}
}

// TestBuildMergeCmd_PortEnvVarOutOfRange pins that an out-of-range $PORT is
// refused: the fallback is resolved before the range is checked.
func TestBuildMergeCmd_PortEnvVarOutOfRange(t *testing.T) {
	t.Setenv("PORT", "99999")
	_, err := buildMergeCmd(&Merge{Client: 3000, Server: 3001})
	if err == nil {
		t.Fatal("expected an error for an out-of-range $PORT, got nil")
	}
	if !strings.Contains(err.Error(), "merge.port") {
		t.Fatalf("error %q does not name merge.port", err)
	}
}

// TestLoad_MergeRouteTargetsAreRangeChecked pins that a route whose target is
// a port number must name a port in 1-65535, refused at its own
// merge.routes[i]; a URL target is passed to merge-port as written.
func TestLoad_MergeRouteTargetsAreRangeChecked(t *testing.T) {
	t.Setenv("PORT", "")
	for _, target := range []string{"99999", "65536", "0", "-1", " 70000 ", "99999999999999999999"} {
		t.Run("refused "+target, func(t *testing.T) {
			_, err := Load(writeConfig(t, "merge:\n  routes:\n    - /=3000\n    - \"/api="+target+"\"\nprocesses:\n  api:\n    cmd: x\n"))
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Code != CodeInvalidMerge || FormatPath(verr.Path) != "merge.routes[1]" {
				t.Fatalf("err = %v, want %s at merge.routes[1]", err, CodeInvalidMerge)
			}
			if !strings.Contains(verr.Msg, "out of range (1-65535)") {
				t.Fatalf("message = %q, want the port range", verr.Msg)
			}
		})
	}
	for _, target := range []string{"1", "65535", "+3001", "03001", "http://api.local:3001", "http://api.local"} {
		t.Run("accepted "+target, func(t *testing.T) {
			cfg := loadFromString(t, "merge:\n  routes:\n    - \"/api="+target+"\"\nprocesses:\n  api:\n    cmd: x\n")
			if cmd := cfg.Processes[MergePortProcessName].Cmd; !strings.Contains(cmd, "--route /api="+target) {
				t.Fatalf("merge-port cmd = %q, want the route as written", cmd)
			}
		})
	}
}
