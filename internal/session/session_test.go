package session

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/paths"
)

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
