package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
)

// freePortPair returns a port nothing is listening on whose successor port is
// free too, so a test can pin what happens when a named port prefers it.
func freePortPair(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if port < 65535 {
			next, nextErr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port+1))
			if nextErr == nil {
				next.Close()
				listener.Close()
				return port
			}
		}
		listener.Close()
	}
	t.Fatal("no free port pair found")
	return 0
}

// TestAcquire_ResourceLabelsAreNormalisedOnce pins that a resource label is
// normalised once, at load time: the whitespace variants of one label are one
// resource, so the session holds one lease and lists one resource. The
// expansion of a trimmed label still applies.
func TestAcquire_ResourceLabelsAreNormalisedOnce(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	body := "processes:\n  api:\n    cmd: serve\nresources: [\"db\", \" db \"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"db"}; !slices.Equal(cfg.Resources, want) {
		t.Fatalf("cfg.Resources = %q, want %q", cfg.Resources, want)
	}
	plan, err := Acquire(cfg, path, "normalised", "normalised", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	if want := []string{"db"}; !slices.Equal(plan.Project.Resources, want) {
		t.Fatalf("Project.Resources = %q, want %q", plan.Project.Resources, want)
	}
	if len(plan.leases) != 2 {
		t.Fatalf("leases = %d, want 2 (the instance lease and one resource lease)", len(plan.leases))
	}

	// A trimmed label still expands.
	body = "processes:\n  api:\n    cmd: serve\nresources: [\" ${config.dir}/db \"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := Acquire(cfg, path, "normalised-expand", "normalised-expand", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan2.Close()
	if want := []string{filepath.Join(dir, "db")}; !slices.Equal(plan2.Project.Resources, want) {
		t.Fatalf("Project.Resources = %q, want %q", plan2.Project.Resources, want)
	}
}

// TestAcquire_ExpandedResourceCollisionIsLeasedOnce pins that two labels that
// expand to the same resource are one lease: the plan does not conflict with
// itself and lists the resource once.
func TestAcquire_ExpandedResourceCollisionIsLeasedOnce(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	absolute := filepath.Join(dir, "db")
	body := fmt.Sprintf("processes:\n  api:\n    cmd: serve\nresources: [\"${config.dir}/db\", %q]\n", absolute)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Acquire(cfg, path, "expand-collision", "expand", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	if want := []string{absolute}; !slices.Equal(plan.Project.Resources, want) {
		t.Fatalf("Project.Resources = %q, want %q", plan.Project.Resources, want)
	}
	if len(plan.leases) != 2 {
		t.Fatalf("leases = %d, want 2 (the instance lease and one resource lease)", len(plan.leases))
	}
}

// TestAcquire_PathResourceLabelsAreCleaned pins the resource identity rule: a
// label that is an absolute path once expanded is cleaned, so its spellings
// are one lease, while every other label is kept exactly as written.
func TestAcquire_PathResourceLabelsAreCleaned(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	db := filepath.Join(dir, "db")
	others := []string{"db", "./db", "tcp:127.0.0.1:5432", "redis://localhost:6379/0", "next-build:" + dir + "//web"}
	cfg := &config.Config{
		// On Windows ${config.dir}/db mixes `\` and `/`.
		Resources: append([]string{"${config.dir}/db", dir + "//db/"}, others...),
		Processes: map[string]config.Process{"api": {Cmd: "serve"}},
	}
	plan, err := Acquire(cfg, path, "clean-paths", "clean", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	want := slices.Sorted(slices.Values(append([]string{db}, others...)))
	if got := slices.Sorted(slices.Values(plan.Project.Resources)); !slices.Equal(got, want) {
		t.Fatalf("Project.Resources = %q, want %q", got, want)
	}
	if want := 1 + len(want); len(plan.leases) != want {
		t.Fatalf("leases = %d, want %d (the instance lease and one per resource)", len(plan.leases), want)
	}
}

// TestAcquire_MergePortIsLeased pins that the merge proxy's fixed port is
// leased: a second session with the same merge port is refused with the
// lease error, instead of both sessions acquiring it.
//
// The config carries the proxy process the merge section generates, so the
// full start really selects the proxy and binds the port.
func TestAcquire_MergePortIsLeased(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePortPair(t)
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	cfg := &config.Config{
		Merge: &config.Merge{Client: 3000, Server: 3001, Port: port},
		Processes: map[string]config.Process{
			"api":                       {Cmd: "serve"},
			config.MergePortProcessName: {Cmd: "merge-port --client 3000 --server 3001 --port " + strconv.Itoa(port)},
		},
	}
	first, err := Acquire(cfg, path, "merge-first", "first", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Close()
	second, err := Acquire(cfg, path, "merge-second", "second", nil)
	if err == nil {
		second.Close()
		t.Fatal("a second session acquired the merge port")
	}
	var lease *LeaseError
	if !errors.As(err, &lease) || lease.Label != "tcp:127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("error = %v, want a *LeaseError for tcp:127.0.0.1:%d", err, port)
	}
	if lease.Instance() {
		t.Fatalf("error = %v, want a resource lease, not the instance lease", err)
	}
}

// TestAcquire_FilteredStartDoesNotLeaseTheMergePort pins that the merge port
// is leased only when the proxy is part of the selected process set.
//
// A filtered start that does not select the proxy never binds the port, so it
// must not hold the lease: a second session that would bind the port has to
// start. A full start still leases it and is still refused.
func TestAcquire_FilteredStartDoesNotLeaseTheMergePort(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePortPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	body := fmt.Sprintf("merge:\n  client: 3000\n  server: 3001\n  port: %d\nprocesses:\n  api:\n    cmd: serve\n", port)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Processes[config.MergePortProcessName]; !ok {
		t.Fatal("the loaded config has no merge-port process")
	}
	filtered, err := Acquire(cfg, path, "merge-filtered", "filtered", []string{"api"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer filtered.Close()
	if slices.Contains(filtered.names, config.MergePortProcessName) {
		t.Fatalf("selected names = %q, want no merge-port proxy", filtered.names)
	}
	lease := leasePath(fmt.Sprintf("tcp:127.0.0.1:%d", port))
	if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge port lease %s exists (err = %v), want no lease for a start that never binds the port", lease, err)
	}
	// A second session that does start the proxy must not be refused.
	full, err := Acquire(cfg, path, "merge-full", "full", nil)
	if err != nil {
		t.Fatalf("a second full session was refused: %v", err)
	}
	defer full.Close()
}

// TestAcquire_MergePortIsLeasedButNotAllocated pins that the merge port is
// leased, never reallocated and never recorded as a named port: the merge
// command keeps its own port, and a managed port that prefers it is moved
// instead of refused.
func TestAcquire_MergePortIsLeasedButNotAllocated(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePortPair(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "proc-compose.yml")
	// The process is not name-matched as a merge upstream ("worker", not
	// "api"), so its named port is only a managed port that prefers the merge
	// port, not a PORT the merge wiring checks against merge.server.
	body := fmt.Sprintf("merge:\n  client: 3000\n  server: 3001\n  port: %d\nprocesses:\n  worker:\n    cmd: serve\n    ports:\n      http: %d\n", port, port)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mergeProc, ok := cfg.Processes["merge-port"]
	if !ok {
		t.Fatal("the loaded config has no merge-port process")
	}
	if len(mergeProc.Ports) != 0 {
		t.Fatalf("merge-port Ports = %v, want none", mergeProc.Ports)
	}
	if want := "--port " + strconv.Itoa(port); !strings.Contains(mergeProc.Cmd, want) {
		t.Fatalf("merge-port cmd = %q, want it to carry %q", mergeProc.Cmd, want)
	}
	plan, err := Acquire(cfg, path, "merge-plan", "merge", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	if _, recorded := plan.Project.Ports["merge-port"]; recorded {
		t.Fatalf("Project.Ports = %v, want no merge-port entry", plan.Project.Ports)
	}
	if got := plan.Project.Ports["worker"]["http"]; got != port+1 {
		t.Fatalf("managed port = %d, want %d (moved, not refused)", got, port+1)
	}
}

// TestAcquire_RefusesAnInvalidFilter pins that the filter is validated before
// anything is leased: an unknown name and a teardown name are refused, no
// lease is left behind, and a valid filter still resolves as before.
func TestAcquire_RefusesAnInvalidFilter(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{
		"api":     {Cmd: "serve"},
		"cleanup": {Cmd: "clean", Mode: config.ProcessModeTeardown},
	}}
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	for _, filter := range []string{"does-not-exist", "cleanup"} {
		plan, err := Acquire(cfg, path, "filter-"+filter, "filter", []string{filter})
		if err == nil {
			plan.Close()
			t.Fatalf("Acquire accepted the filter %q", filter)
		}
		if !strings.Contains(err.Error(), filter) {
			t.Fatalf("error = %v, want it to name %q", err, filter)
		}
		entries, readErr := os.ReadDir(paths.LeaseDir())
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("a refused filter left %d lease files behind", len(entries))
		}
	}

	// A valid filter behaves exactly as before.
	plan, err := Acquire(cfg, path, "filter-valid", "filter", []string{"api"})
	if err != nil {
		t.Fatalf("Acquire with a valid filter: %v", err)
	}
	plan.Close()

	// The dependency closure is unchanged: an unknown dependency is still
	// dropped, and the requested process is still selected.
	closure := &config.Config{Processes: map[string]config.Process{"api": {Cmd: "serve", DependsOn: []string{"missing"}}}}
	plan2, err := Acquire(closure, path, "filter-closure", "closure", []string{"api"})
	if err != nil {
		t.Fatalf("Acquire with an unknown dependency: %v", err)
	}
	defer plan2.Close()
	if !slices.Contains(plan2.names, "api") || slices.Contains(plan2.names, "missing") {
		t.Fatalf("selected names = %q, want the requested process only", plan2.names)
	}
}

// TestSave_ReadNeverSeesAPartialDocument pins that the session metadata is
// written atomically: a reader that reads while the writer saves never
// observes a truncated document.
func TestSave_ReadNeverSeesAPartialDocument(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := paths.Metadata("atomic")
	project := &ipc.Project{
		Session:    "atomic",
		RunID:      ipc.NewRunID(),
		ConfigPath: "/tmp/proc-compose.yml",
		Ports:      map[string]map[string]int{"api": {"http": 4100}},
	}
	if err := Save(path, project); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var failures atomic.Int64
	var reads atomic.Int64
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := Read(path); err != nil {
					failures.Add(1)
				}
				reads.Add(1)
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if err := Save(path, project); err != nil {
			close(stop)
			writers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	writers.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d of %d reads failed while the metadata was written", failures.Load(), reads.Load())
	}
	if reads.Load() == 0 {
		t.Fatal("no read observed the metadata")
	}
}

// TestRead_RejectsDocumentWithoutSession pins that a metadata document that
// describes no session is refused, and that a document naming either the run
// or the config is still read.
func TestRead_RejectsDocumentWithoutSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, body := range []string{"null", "{}", "null\n"} {
		path := write("pc-empty.meta.json", body)
		project, err := Read(path)
		if err == nil {
			t.Fatalf("Read(%q) = %+v, want an error", body, project)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v, want it to name the file", err)
		}
	}
	for _, body := range []string{`{"run_id":"run-1"}`, `{"config_path":"/tmp/proc-compose.yml"}`} {
		path := write("pc-session.meta.json", body)
		if _, err := Read(path); err != nil {
			t.Fatalf("Read(%q): %v", body, err)
		}
	}
}

// TestResolve_AcceptsReorderedResources pins that the order of the resource
// labels is not a resource-set change: a pure reordering resolves, while
// adding or removing a resource is still refused with the same message.
func TestResolve_AcceptsReorderedResources(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	processes := map[string]config.Process{"api": {Cmd: "serve"}}
	cfg := &config.Config{Resources: []string{"alpha", "beta"}, Processes: processes}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "reorder", "reorder", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	reordered := &config.Config{Resources: []string{"beta", "alpha"}, Processes: processes}
	if _, err := plan.Resolve(reordered); err != nil {
		t.Fatalf("a pure reorder was refused: %v", err)
	}
	for _, changed := range [][]string{{"alpha"}, {"alpha", "beta", "gamma"}} {
		other := &config.Config{Resources: changed, Processes: processes}
		_, err := plan.Resolve(other)
		if err == nil {
			t.Fatalf("a resource-set change to %q was accepted", changed)
		}
		if err.Error() != "resources changed; restart the session" {
			t.Fatalf("error = %v, want the unchanged message", err)
		}
	}
}

// TestResolve_RefusesAMergePortChange pins that a reload cannot move the merge
// port the session leases: the proxy would restart on a port no lease covers.
// The same port resolves, and so does a reload that drops the merge section,
// which changes no lease. A session that does not run the proxy leases no
// merge port, so a change there is not refused.
func TestResolve_RefusesAMergePortChange(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	port := freePortPair(t)
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	withMerge := func(port int) *config.Config {
		return &config.Config{
			Merge: &config.Merge{Client: 3000, Server: 3001, Port: port},
			Processes: map[string]config.Process{
				"api":                       {Cmd: "serve"},
				config.MergePortProcessName: {Cmd: "merge-port --client 3000 --server 3001 --port " + strconv.Itoa(port)},
			},
		}
	}
	plan, err := Acquire(withMerge(port), path, "merge-reload", "merge-reload", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	if _, err := plan.Resolve(withMerge(port)); err != nil {
		t.Fatalf("an unchanged merge port was refused: %v", err)
	}
	_, err = plan.Resolve(withMerge(port + 1))
	if err == nil {
		t.Fatal("a reload moved the leased merge port")
	}
	if err.Error() != "merge port changed; restart the session" {
		t.Fatalf("error = %v, want the merge port refusal", err)
	}
	if _, err := plan.Resolve(&config.Config{Processes: map[string]config.Process{"api": {Cmd: "serve"}}}); err != nil {
		t.Fatalf("a reload that drops the merge section was refused: %v", err)
	}

	filtered, err := Acquire(withMerge(port), path, "merge-reload-filtered", "filtered", []string{"api"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer filtered.Close()
	if _, err := filtered.Resolve(withMerge(port + 1)); err != nil {
		t.Fatalf("a session without the proxy refused a merge port change: %v", err)
	}
}

func TestSessionLeaseHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--session-lease-helper" {
		return
	}
	root := os.Args[len(os.Args)-1]
	cfg := &config.Config{Resources: []string{"crash-release"}, Processes: map[string]config.Process{"api": {Cmd: "serve"}}}
	plan, err := Acquire(cfg, filepath.Join(root, "proc-compose.yml"), "child-owner", "child", nil)
	if err != nil {
		os.Exit(2)
	}
	// Deliberately leave the lease open: process exit must release it even
	// when application-level Close is never reached.
	if len(plan.leases) == 0 {
		os.Exit(3)
	}
	_, _ = os.Stdout.Write([]byte("R"))
	var release [1]byte
	_, _ = os.Stdin.Read(release[:])
	os.Exit(0)
}

func TestAcquire_ProcessExitReleasesResourceLease(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionLeaseHelper$", "--", "--session-lease-helper", root)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			cancel()
			_ = child.Wait()
		}
	}()
	var ready [1]byte
	if _, err := io.ReadFull(output, ready[:]); err != nil || ready[0] != 'R' {
		t.Fatalf("lease owner failed to start: %v", err)
	}
	cfg := &config.Config{Resources: []string{"crash-release"}, Processes: map[string]config.Process{"api": {Cmd: "serve"}}}
	path := filepath.Join(root, "proc-compose.yml")
	if plan, err := Acquire(cfg, path, "parent-owner", "parent", nil); err == nil {
		plan.Close()
		t.Fatal("lease did not exclude another process")
	}
	input.Close()
	err = child.Wait()
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Acquire(cfg, path, "parent-owner", "parent", nil)
	if err != nil {
		t.Fatalf("process exit left an orphaned lease: %v", err)
	}
	plan.Close()
}

// bindable reports whether another program could bind port on the loopback
// address the plan allocates on.
func bindable(port int) bool {
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// TestAcquire_HoldsAllocatedPortsUntilTheProcessStarts pins that an allocated
// port stays bound from Acquire until a process that uses it is about to
// start, so no other program can take it in between. ReleasePorts frees the
// ports the process declares and the ones its fields reference, and leaves the
// rest held until Close.
func TestAcquire_HoldsAllocatedPortsUntilTheProcessStarts(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{
		"api":    {Cmd: "serve", Ports: map[string]int{"http": 0}},
		"web":    {Cmd: "web", Ports: map[string]int{"http": 0}, Env: map[string]string{"API_URL": "${urls.api.http}"}},
		"db":     {Cmd: "db", Ports: map[string]int{"pg": 0}},
		"worker": {Cmd: "work"},
	}}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "held-ports", "held", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	api, web, db := plan.Project.Ports["api"]["http"], plan.Project.Ports["web"]["http"], plan.Project.Ports["db"]["pg"]
	held := func(when string, ports ...int) {
		t.Helper()
		for _, port := range ports {
			if bindable(port) {
				t.Fatalf("%s: port %d leased by the plan was bindable by another program", when, port)
			}
		}
	}
	free := func(when string, ports ...int) {
		t.Helper()
		for _, port := range ports {
			if !bindable(port) {
				t.Fatalf("%s: port %d is still held", when, port)
			}
		}
	}
	held("after Acquire", api, web, db)
	plan.ReleasePorts("worker")
	plan.ReleasePorts("not-a-process")
	held("after releasing processes that use no port", api, web, db)
	plan.ReleasePorts("web")
	free("after web's release", web, api)
	held("after web's release", db)
	plan.ReleasePorts("web")
	plan.Close()
	free("after Close", db)
}

func TestSessionPortHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--session-port-helper" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+os.Args[len(os.Args)-1])
	if err != nil {
		_, _ = os.Stdout.Write([]byte("E"))
		os.Exit(2)
	}
	_, _ = os.Stdout.Write([]byte("R"))
	var release [1]byte
	_, _ = os.Stdin.Read(release[:])
	listener.Close()
	os.Exit(0)
}

// TestReleasePorts_ProcessBindsItsPort pins that a process started after
// ReleasePorts binds its port, and that a process started while another port
// was held does not keep that port: once released it binds while the earlier
// process still runs. The held listener must not leak into a child (handle
// inheritance on Windows, a missing close-on-exec on Unix).
func TestReleasePorts_ProcessBindsItsPort(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{
		"api": {Cmd: "serve", Ports: map[string]int{"http": 0}},
		"web": {Cmd: "web", Ports: map[string]int{"http": 0}},
	}}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "bind-after-release", "bind", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	api, web := plan.Project.Ports["api"]["http"], plan.Project.Ports["web"]["http"]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plan.ReleasePorts("web")
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionPortHelper$", "--", "--session-port-helper", strconv.Itoa(web))
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		_ = child.Wait()
	}()
	var bound [1]byte
	if _, err := io.ReadFull(output, bound[:]); err != nil || bound[0] != 'R' {
		t.Fatalf("the process could not bind its port %d after ReleasePorts (reply %q, err %v)", web, bound[0], err)
	}

	plan.ReleasePorts("api")
	if !bindable(api) {
		t.Fatalf("port %d stayed bound after its release while a process started during its hold runs", api)
	}
}

func TestAcquire_IndependentEndpointsAndReferences(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := &config.Config{Processes: map[string]config.Process{
		"api": {Cmd: "serve ${ports.api.http}", Ports: map[string]int{"http": base}},
		"web": {Cmd: "web", DependsOn: []string{"api"}, Env: map[string]string{"API_URL": "${urls.api.http}", "UPLOAD_ROOT": "${session.dir}/uploads"}},
	}}
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	firstHash, _ := paths.FromConfig(path, "first")
	secondHash, _ := paths.FromConfig(path, "second")
	first, err := Acquire(cfg, path, firstHash, "first", []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Acquire(cfg, path, secondHash, "second", []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.Project.Ports["api"]["http"] == second.Project.Ports["api"]["http"] {
		t.Fatal("sessions leased the same port")
	}
	for _, plan := range []*Plan{first, second} {
		port := plan.Project.Ports["api"]["http"]
		if plan.Config.Processes["web"].Env["API_URL"] != "http://127.0.0.1:"+strconv.Itoa(port) || !strings.HasSuffix(plan.Config.Processes["api"].Cmd, strconv.Itoa(port)) {
			t.Fatal("endpoint references disagree")
		}
		if !strings.HasPrefix(plan.Config.Processes["web"].Env["UPLOAD_ROOT"], plan.Project.Directory) {
			t.Fatal("runtime path escaped its session")
		}
	}
}

func TestAcquire_ConcurrentStartsHaveOneOwner(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, shared := range []bool{false, true} {
		t.Run(strconv.FormatBool(shared), func(t *testing.T) {
			cfg := &config.Config{Processes: map[string]config.Process{"api": {Cmd: "serve", Ports: map[string]int{"http": 0}}}}
			if shared {
				cfg.Resources = []string{"shared-db"}
			}
			path := filepath.Join(t.TempDir(), "proc-compose.yml")
			start := make(chan struct{})
			plans := make(chan *Plan, 8)
			var workers sync.WaitGroup
			for i := 0; i < 8; i++ {
				workers.Add(1)
				go func(i int) {
					defer workers.Done()
					<-start
					hash := "same-instance"
					if shared {
						hash = "distinct-" + strconv.Itoa(i)
					}
					plan, err := Acquire(cfg, path, hash, hash, nil)
					if err == nil {
						plans <- plan
					}
				}(i)
			}
			close(start)
			workers.Wait()
			close(plans)
			count := 0
			for plan := range plans {
				count++
				plan.Close()
			}
			if count != 1 {
				t.Fatalf("expected one owner, got %d", count)
			}
		})
	}
}

func TestAcquire_ResourceContentionAndRelease(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Resources: []string{"postgresql:role-ddl"}, Processes: map[string]config.Process{"api": {Cmd: "serve"}}}
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	first, err := Acquire(cfg, path, "first", "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Acquire(cfg, path, "second", "second", nil); err == nil {
		second.Close()
		t.Fatal("shared resource admitted two owners")
	}
	first.Close()
	second, err := Acquire(cfg, path, "second", "second", nil)
	if err != nil {
		t.Fatalf("resource not released: %v", err)
	}
	second.Close()
}

func TestResolve_RejectsLeaseChangesAndUnknownReferences(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{"api": {Cmd: "serve", Ports: map[string]int{"http": 0}}}}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "lease", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	cfg.Processes["api"] = config.Process{Cmd: "serve", Ports: map[string]int{"http": 1234}}
	if _, err := plan.Resolve(cfg); err == nil {
		t.Fatal("reload changed port ownership")
	}
	cfg.Processes["api"] = config.Process{Cmd: "serve ${ports.missing.http}", Ports: map[string]int{"http": 0}}
	if _, err := plan.Resolve(cfg); err == nil {
		t.Fatal("unknown endpoint reference accepted")
	}
}

// TestResolve_ExpandsProbeCmd pins that a ready_when.cmd or health_when probe
// receives the same endpoint references as the process's own cmd, and that an
// unresolved one is refused rather than run literally.
func TestResolve_ExpandsProbeCmd(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{"api": {
		Cmd:       "serve",
		Ports:     map[string]int{"http": 0},
		ReadyWhen: &config.ReadyWhen{Cmd: "check ${urls.api.http}/ready ${session.name}"},
	}}}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "probe-cmd", "probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	port := plan.Project.Ports["api"]["http"]
	if got, want := plan.Config.Processes["api"].ReadyWhen.Cmd, "check http://127.0.0.1:"+strconv.Itoa(port)+"/ready probe"; got != want {
		t.Fatalf("probe cmd = %q, want %q", got, want)
	}
	if cfg.Processes["api"].ReadyWhen.Cmd != "check ${urls.api.http}/ready ${session.name}" {
		t.Fatal("resolving mutated the source config's probe")
	}
	cfg.Processes["api"] = config.Process{Cmd: "serve", Ports: map[string]int{"http": 0}, ReadyWhen: &config.ReadyWhen{Cmd: "check ${ports.missing.http}"}}
	if _, err := plan.Resolve(cfg); err == nil {
		t.Fatal("unresolved probe cmd reference accepted")
	}

	// health_when gets the same expansion, on every probe kind.
	cfg.Processes["api"] = config.Process{Cmd: "serve", Ports: map[string]int{"http": 0}, HealthWhen: &config.ReadyWhen{Cmd: "live ${urls.api.http} ${session.name}"}}
	resolved, err := plan.Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved.Processes["api"].HealthWhen.Cmd, "live http://127.0.0.1:"+strconv.Itoa(port)+" probe"; got != want {
		t.Fatalf("health cmd = %q, want %q", got, want)
	}
	cfg.Processes["api"] = config.Process{Cmd: "serve", Ports: map[string]int{"http": 0}, HealthWhen: &config.ReadyWhen{HTTP: "${urls.missing.http}/live"}}
	if _, err := plan.Resolve(cfg); err == nil {
		t.Fatal("unresolved health_when reference accepted")
	}
}

// TestResolve_ExpandsTeardownUnderFilter pins that a teardown process gets
// the session's expansion and environment even when a filter started only part
// of the stack: teardown runs for the whole stack.
func TestResolve_ExpandsTeardownUnderFilter(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{
		"api":     {Cmd: "serve", Ports: map[string]int{"http": 0}},
		"worker":  {Cmd: "work"},
		"cleanup": {Cmd: "clean ${session.dir}", Mode: config.ProcessModeTeardown, Env: map[string]string{"API": "${urls.api.http}"}},
	}}
	plan, err := Acquire(cfg, filepath.Join(t.TempDir(), "proc-compose.yml"), "teardown-filter", "td", []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	got := plan.Config.Processes["cleanup"]
	if want := "clean " + plan.Project.Directory; got.Cmd != want {
		t.Fatalf("teardown cmd = %q, want %q", got.Cmd, want)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(plan.Project.Ports["api"]["http"]); got.Env["API"] != want {
		t.Fatalf("teardown env API = %q, want %q", got.Env["API"], want)
	}
	if got.Env["PROC_COMPOSE_SESSION"] != "td" || got.Env["PROC_COMPOSE_RUN_ID"] != plan.Project.RunID {
		t.Fatalf("teardown env = %v, want the session variables", got.Env)
	}
	// The session metadata names them, so stop can allow for their budget.
	if !slices.Equal(plan.Project.Teardown, []string{"cleanup"}) {
		t.Fatalf("Project.Teardown = %v, want [cleanup]", plan.Project.Teardown)
	}
}

// TestLeaseInstance_BusyWhileHeld pins the lease crash recovery runs under: it
// is the same instance lease Acquire holds for a run's lifetime, it is refused
// without blocking while any owner holds it, and releasing it frees it.
func TestLeaseInstance_BusyWhileHeld(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{"api": {Cmd: "serve"}}}
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	plan, err := Acquire(cfg, path, "held", "held", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	release, err := LeaseInstance("held")
	if err == nil {
		release()
		plan.Close()
		t.Fatal("LeaseInstance admitted a second owner while Acquire held the instance lease")
	}
	var lease *LeaseError
	if !errors.As(err, &lease) || !lease.Instance() {
		t.Fatalf("LeaseInstance error = %v, want an instance *LeaseError", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("LeaseInstance blocked for %s on a held lease", elapsed)
	}
	plan.Close()

	release, err = LeaseInstance("held")
	if err != nil {
		t.Fatalf("LeaseInstance after the owner closed: %v", err)
	}
	if held, _ := InstanceHeld("held"); !held {
		t.Fatal("InstanceHeld = false while LeaseInstance holds the lease")
	}
	if _, err := Acquire(cfg, path, "held", "held", nil); err == nil {
		t.Fatal("Acquire admitted an owner while LeaseInstance held the instance lease")
	}
	release()
	if held, _ := InstanceHeld("held"); held {
		t.Fatal("instance lease still held after release")
	}
}

// TestLeaseInstance_CreatesLeaseDir covers recovery on a host where no run ever
// created the lease directory, such as after a reboot cleared the runtime dir.
func TestLeaseInstance_CreatesLeaseDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "fresh"))
	release, err := LeaseInstance("fresh")
	if err != nil {
		t.Fatalf("LeaseInstance without a lease dir: %v", err)
	}
	release()
}

// TestAcquire_DottedProcessNameExpandsItsPort pins that the session expands
// the reference of a dotted process name to that process's port. The plan
// splits the reference name at the last dot too, so the two agree: a dot in a
// port name would break that, which is why the loader refuses one.
func TestAcquire_DottedProcessNameExpandsItsPort(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := &config.Config{Processes: map[string]config.Process{
		"a.b": {Cmd: "serve ${ports.a.b.c}", Ports: map[string]int{"c": 0}},
	}}
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	hash, _ := paths.FromConfig(path, "dotted")
	plan, err := Acquire(cfg, path, hash, "dotted", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer plan.Close()
	port := plan.Project.Ports["a.b"]["c"]
	if port == 0 {
		t.Fatalf("Project.Ports = %v, want a leased port for a.b/c", plan.Project.Ports)
	}
	if want := "serve " + strconv.Itoa(port); plan.Config.Processes["a.b"].Cmd != want {
		t.Fatalf("cmd = %q, want %q", plan.Config.Processes["a.b"].Cmd, want)
	}
}
