package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
	"gopkg.in/yaml.v3"
)

// capturingVerifier records the config path it was asked to verify and the
// mode of that file while it existed. It loads the config it was handed so
// the caller can inspect how the copy resolves.
type capturingVerifier struct {
	path string
	mode os.FileMode
	cfg  *config.Config
	err  error
}

func (c *capturingVerifier) Verify(configPath string) (*Verification, error) {
	c.path = configPath
	if info, err := os.Stat(configPath); err == nil {
		c.mode = info.Mode().Perm()
	}
	c.cfg, c.err = config.Load(configPath)
	return &Verification{ValidationStatus: "passed"}, nil
}

// writeMergeProject writes a project with a root-level frontend and a backend
// in api/, so the doctor's suggestion carries a merge: section and the
// suggested dirs have to resolve against the project root.
func writeMergeProject(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	for _, sub := range []string{"", "api"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	writeFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"latest"}}`)
	writeFile(t, filepath.Join(dir, ".env.example"), "VITE_API_URL=http://localhost:5173/api\n")
	writeFile(t, filepath.Join(dir, "api", "package.json"), `{"scripts":{"dev":"node server.js"}}`)
	writeFile(t, filepath.Join(dir, "api", ".env.example"), "PORT=3001\n")
	writeFile(t, filepath.Join(dir, "api", "server.js"), "app.get('/api/users', handler)\n")
	return dir
}

// TestRunVerifyDoesNotWriteIntoProjectRoot: bootstrap is safe and read-only
// by default, so the verification copy must not land in the project.
func TestRunVerifyDoesNotWriteIntoProjectRoot(t *testing.T) {
	dir := writeMergeProject(t)
	before := dirEntries(t, dir)

	verifier := &capturingVerifier{}
	if _, err := Run(Options{Root: dir, Verify: true, Verifier: verifier}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if after := dirEntries(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("project root changed: before %v, after %v", before, after)
	}
}

// TestRunVerifyResolvesDirsLikeTheProjectConfig: the verified copy lives in
// the temp dir, so its dir values have to be rewritten to absolute paths.
// Otherwise the copy resolves against the temp dir and the merge topology it
// reports is not the one that will run.
func TestRunVerifyResolvesDirsLikeTheProjectConfig(t *testing.T) {
	dir := writeMergeProject(t)
	// Keep the merge port deterministic.
	t.Setenv("PORT", "")

	verifier := &capturingVerifier{}
	if _, err := Run(Options{Root: dir, Verify: true, Verifier: verifier}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if verifier.path == "" {
		t.Fatal("expected the verifier to receive a config path")
	}
	if verifier.err != nil {
		t.Fatalf("config.Load(verified copy) returned error: %v", verifier.err)
	}
	cfg := verifier.cfg
	api, ok := cfg.Processes["api"]
	if !ok {
		t.Fatalf("verified copy has no api process: %+v", cfg.Processes)
	}
	if want := filepath.Join(dir, "api"); api.Dir != want {
		t.Fatalf("api dir = %q, want %q", api.Dir, want)
	}
	if len(cfg.Merge.ApiPrefixes) != 1 || cfg.Merge.ApiPrefixes[0] != "/api" {
		t.Fatalf("merge api_prefixes = %v, want [/api]", cfg.Merge.ApiPrefixes)
	}

	clientName := ""
	for name, proc := range cfg.Processes {
		if proc.Env["PORT"] == "5173" {
			clientName = name
		}
	}
	if clientName == "" {
		t.Fatalf("verified copy has no frontend process: %+v", cfg.Processes)
	}
	client := cfg.Processes[clientName]
	if client.Dir != dir {
		t.Fatalf("client dir = %q, want %q", client.Dir, dir)
	}
	if got := client.Env["VITE_API_URL"]; got != "http://localhost:8080/api" {
		t.Fatalf("client VITE_API_URL = %q, want %q", got, "http://localhost:8080/api")
	}
}

// TestRunVerifyRemovesTemporaryConfigCopy: the verification copy is removed
// after the run and is only readable by its owner.
func TestRunVerifyRemovesTemporaryConfigCopy(t *testing.T) {
	dir := writeMergeProject(t)

	verifier := &capturingVerifier{}
	if _, err := Run(Options{Root: dir, Verify: true, Verifier: verifier}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if verifier.path == "" {
		t.Fatal("expected the verifier to receive a config path")
	}
	if _, err := os.Stat(verifier.path); !os.IsNotExist(err) {
		t.Fatalf("temporary config copy was not removed: %v", err)
	}
	// Windows does not expose POSIX file permission bits.
	if runtime.GOOS != "windows" && verifier.mode != 0600 {
		t.Fatalf("temporary config copy mode = %o, want 600", verifier.mode)
	}
}

// TestAbsProcessDirsRewritesOnlyDir: the rewrite touches dir and nothing
// else, and a process without a dir key gets one because such a process
// resolves to the config file's directory.
func TestAbsProcessDirsRewritesOnlyDir(t *testing.T) {
	contents := []byte(`merge:
  client: 5173
  server: 3001
processes:
  api:
    cmd: npm run dev
    dir: ./api
    env:
      PORT: "3001"
    env_file: ./api/.env
  web:
    cmd: npm run dev
`)
	out, err := absProcessDirs(contents, "/base")
	if err != nil {
		t.Fatalf("absProcessDirs returned error: %v", err)
	}

	var before, after config.Config
	if err := yaml.Unmarshal(contents, &before); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := yaml.Unmarshal(out, &after); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if !reflect.DeepEqual(before.Merge, after.Merge) {
		t.Fatalf("merge changed: before %+v, after %+v", before.Merge, after.Merge)
	}
	if len(before.Processes) != len(after.Processes) {
		t.Fatalf("process set changed: before %d, after %d", len(before.Processes), len(after.Processes))
	}
	for name, proc := range before.Processes {
		got := after.Processes[name]
		if got.Cmd != proc.Cmd {
			t.Fatalf("process %q cmd changed: %q -> %q", name, proc.Cmd, got.Cmd)
		}
		if got.EnvFile != proc.EnvFile {
			t.Fatalf("process %q env_file changed: %q -> %q", name, proc.EnvFile, got.EnvFile)
		}
		if !reflect.DeepEqual(got.Env, proc.Env) {
			t.Fatalf("process %q env changed: %+v -> %+v", name, proc.Env, got.Env)
		}
	}
	if got := after.Processes["api"].Dir; got != filepath.Join("/base", "api") {
		t.Fatalf("api dir = %q, want %q", got, filepath.Join("/base", "api"))
	}
	if got := after.Processes["web"].Dir; got != "/base" {
		t.Fatalf("web dir = %q, want %q", got, "/base")
	}
}

// TestAbsProcessDirsKeepsAbsoluteDirs: an absolute dir is already resolved
// and must not be rewritten.
func TestAbsProcessDirsKeepsAbsoluteDirs(t *testing.T) {
	// /srv/api has no volume name, so it is not absolute on Windows; Abs gives
	// this platform's absolute form of it (/srv/api itself on Unix).
	abs, err := filepath.Abs(filepath.FromSlash("/srv/api"))
	if err != nil {
		t.Fatalf("absolute dir: %v", err)
	}
	out, err := absProcessDirs([]byte("processes:\n  api:\n    cmd: npm run dev\n    dir: "+strconv.Quote(abs)+"\n"), "/base")
	if err != nil {
		t.Fatalf("absProcessDirs returned error: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if got := cfg.Processes["api"].Dir; got != abs {
		t.Fatalf("api dir = %q, want %q", got, abs)
	}
}

// TestAbsProcessDirsLeavesConfigWithoutProcesses: a config without processes
// has nothing to rewrite.
func TestAbsProcessDirsLeavesConfigWithoutProcesses(t *testing.T) {
	contents := []byte("merge:\n  client: 5173\n  server: 3001\n")
	out, err := absProcessDirs(contents, "/base")
	if err != nil {
		t.Fatalf("absProcessDirs returned error: %v", err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if cfg.Merge == nil || cfg.Merge.Client != 5173 {
		t.Fatalf("merge changed: %+v", cfg.Merge)
	}
}

// TestAbsProcessDirsRejectsUnparseableConfig: a config that does not parse
// is reported as an error so the caller can fall back to the original.
func TestAbsProcessDirsRejectsUnparseableConfig(t *testing.T) {
	if _, err := absProcessDirs([]byte("processes: [\n"), "/base"); err == nil {
		t.Fatal("expected an error for an unparseable config")
	}
}

// dirEntries lists the names of the entries in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
