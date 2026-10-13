// Package session resolves a stack's declared endpoints and leases resources.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
)

type Plan struct {
	Project   *ipc.Project
	Config    *config.Config
	leases    []*os.File
	variables map[string]string
	preferred map[string]map[string]int
	resources []string
	names     []string
}

func Acquire(cfg *config.Config, configPath, hash, name string, filter []string) (_ *Plan, err error) {
	p := &Plan{
		Project:   &ipc.Project{Session: name, RunID: ipc.NewRunID(), ConfigPath: configPath, Directory: paths.SessionDir(hash), Ports: make(map[string]map[string]int), URLs: make(map[string]map[string]string), JournalPath: paths.Journal(hash)},
		variables: make(map[string]string), preferred: make(map[string]map[string]int), resources: slices.Clone(cfg.Resources),
		// Teardown processes run for the whole stack, whatever the filter.
		names: append(config.SelectedNames(cfg, filter), config.TeardownNames(cfg)...),
	}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if err = os.MkdirAll(p.Project.Directory, 0700); err != nil {
		return nil, err
	}
	if err = p.lease("instance:" + hash); err != nil {
		return nil, err
	}
	p.variables["session.name"] = name
	if name == "" {
		p.variables["session.name"] = "default"
	}
	p.Project.Teardown = config.TeardownNames(cfg)
	p.variables["session.id"] = hash
	p.variables["session.dir"] = p.Project.Directory
	p.variables["config.dir"] = filepath.Dir(configPath)
	resources := slices.Clone(cfg.Resources)
	sort.Strings(resources)
	for _, label := range resources {
		resolved, expandErr := p.expand(label)
		if expandErr != nil {
			return nil, expandErr
		}
		if err = p.lease("resource:" + resolved); err != nil {
			return nil, err
		}
		p.Project.Resources = append(p.Project.Resources, resolved)
	}
	for _, process := range p.names {
		declared := cfg.Processes[process].Ports
		p.preferred[process] = maps.Clone(declared)
		portNames := make([]string, 0, len(declared))
		for portName := range declared {
			portNames = append(portNames, portName)
		}
		sort.Strings(portNames)
		for _, portName := range portNames {
			port, allocateErr := p.allocate(declared[portName])
			if allocateErr != nil {
				return nil, fmt.Errorf("%s.%s: %w", process, portName, allocateErr)
			}
			if p.Project.Ports[process] == nil {
				p.Project.Ports[process] = make(map[string]int)
				p.Project.URLs[process] = make(map[string]string)
			}
			url := fmt.Sprintf("http://127.0.0.1:%d", port)
			p.Project.Ports[process][portName], p.Project.URLs[process][portName] = port, url
			p.variables["ports."+process+"."+portName] = strconv.Itoa(port)
			p.variables["urls."+process+"."+portName] = url
		}
	}
	p.Config, err = p.Resolve(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Resolve also applies reloads against the already-owned endpoint assignments.
// Changing leases requires a new session, rather than silently changing owners.
func (p *Plan) Resolve(cfg *config.Config) (*config.Config, error) {
	if !slices.Equal(p.resources, cfg.Resources) {
		return nil, fmt.Errorf("resources changed; restart the session")
	}
	result := *cfg
	result.Processes = maps.Clone(cfg.Processes)
	for _, name := range p.names {
		proc, exists := cfg.Processes[name]
		if !exists {
			continue
		}
		if !maps.Equal(proc.Ports, p.preferred[name]) {
			return nil, fmt.Errorf("named ports for %q changed; restart the session", name)
		}
		var err error
		proc.Cmd, err = p.expand(proc.Cmd)
		if err != nil {
			return nil, fmt.Errorf("process %q cmd: %w", name, err)
		}
		proc.Env = maps.Clone(proc.Env)
		if proc.Env == nil {
			proc.Env = make(map[string]string)
		}
		for key, value := range proc.Env {
			proc.Env[key], err = p.expand(value)
			if err != nil {
				return nil, fmt.Errorf("process %q env key %q: %w", name, key, err)
			}
		}
		if port, ok := p.Project.Ports[name]["http"]; ok {
			if _, explicit := proc.Env["PORT"]; !explicit {
				proc.Env["PORT"] = strconv.Itoa(port)
			}
		}
		proc.Env["PROC_COMPOSE_SESSION"] = p.variables["session.name"]
		proc.Env["PROC_COMPOSE_SESSION_DIR"] = p.Project.Directory
		proc.Env["PROC_COMPOSE_RUN_ID"] = p.Project.RunID
		proc.Dir, err = p.expand(proc.Dir)
		if err != nil {
			return nil, fmt.Errorf("process %q dir: %w", name, err)
		}
		if proc.Dir == "" && p.Project.Session != "" {
			proc.Dir = filepath.Dir(p.Project.ConfigPath)
		}
		if proc.Dir != "" && !filepath.IsAbs(proc.Dir) {
			proc.Dir = filepath.Join(filepath.Dir(p.Project.ConfigPath), proc.Dir)
		}
		if proc.ReadyWhen, err = p.expandProbe(proc.ReadyWhen); err != nil {
			return nil, fmt.Errorf("process %q probe: %w", name, err)
		}
		if proc.HealthWhen, err = p.expandProbe(proc.HealthWhen); err != nil {
			return nil, fmt.Errorf("process %q health_when: %w", name, err)
		}
		result.Processes[name] = proc
	}
	return &result, nil
}

// expandProbe returns a copy of probe with its endpoint references resolved,
// or nil for no probe. A log probe is a regex and is left as written.
func (p *Plan) expandProbe(probe *config.ReadyWhen) (*config.ReadyWhen, error) {
	if probe == nil {
		return nil, nil
	}
	expanded := *probe
	var err error
	for _, field := range []*string{&expanded.HTTP, &expanded.TCP, &expanded.Cmd} {
		if *field, err = p.expand(*field); err != nil {
			return nil, err
		}
	}
	return &expanded, nil
}

func (p *Plan) expand(value string) (string, error) {
	for name, replacement := range p.variables {
		value = strings.ReplaceAll(value, "${"+name+"}", replacement)
	}
	for _, namespace := range []string{"ports.", "urls.", "session.", "config."} {
		if strings.Contains(value, "${"+namespace) {
			return "", fmt.Errorf("unresolved %s reference (select its dependency or declare its named port)", strings.TrimSuffix(namespace, "."))
		}
	}
	return value, nil
}

// LeaseError reports a lease already held by another owner. Label is the
// leased identity, for example "instance:<hash>" or "tcp:127.0.0.1:3000".
type LeaseError struct {
	Label string
	Err   error
}

func (e *LeaseError) Error() string {
	return fmt.Sprintf("resource %q is owned by another session: %v", e.Label, e.Err)
}

func (e *LeaseError) Unwrap() error { return e.Err }

// Instance reports whether the lease is the session identity itself, rather
// than a named resource or endpoint.
func (e *LeaseError) Instance() bool { return strings.HasPrefix(e.Label, "instance:") }

func leasePath(label string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	return filepath.Join(paths.LeaseDir(), fmt.Sprintf("%016x.lock", h.Sum64()))
}

// acquireLease takes the named lease without blocking and returns the open
// lock file that holds it; closing the file releases it. A lease another owner
// holds is a *LeaseError.
func acquireLease(label string) (*os.File, error) {
	if err := os.MkdirAll(paths.LeaseDir(), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(leasePath(label), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lock(file); err != nil {
		file.Close()
		return nil, &LeaseError{Label: label, Err: err}
	}
	return file, nil
}

func (p *Plan) lease(label string) error {
	file, err := acquireLease(label)
	if err != nil {
		return err
	}
	p.leases = append(p.leases, file)
	return nil
}

// LeaseInstance takes the session's instance lease, the one Acquire holds for a
// run's whole lifetime, without blocking. Holding it excludes every other
// command that would start or reclaim the session, so crash recovery can act on
// the PID file without racing a fresh `up`. A held lease is an instance
// *LeaseError. release frees the lease.
func LeaseInstance(hash string) (release func(), err error) {
	file, err := acquireLease("instance:" + hash)
	if err != nil {
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

// InstanceHeld observes whether some owner holds the session's instance lease.
// When the lease is free it is taken and released immediately; this is an
// observation for classification, never an acquisition that outlives the call.
func InstanceHeld(hash string) (bool, error) {
	file, err := os.OpenFile(leasePath("instance:"+hash), os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := lock(file); err != nil {
		return true, nil
	}
	return false, nil
}

func (p *Plan) allocate(preferred int) (int, error) {
	for attempt := 0; attempt < 128; attempt++ {
		candidate := preferred
		if candidate != 0 {
			candidate += attempt
			if candidate > 65535 {
				break
			}
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err != nil {
			continue
		}
		port := listener.Addr().(*net.TCPAddr).Port
		err = p.lease(fmt.Sprintf("tcp:127.0.0.1:%d", port))
		listener.Close()
		if err == nil {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available named port starting at %d", preferred)
}

func (p *Plan) Close() {
	for i := len(p.leases) - 1; i >= 0; i-- {
		_ = p.leases[i].Close()
	}
	p.leases = nil
}

func Save(path string, project *ipc.Project) error {
	data, err := json.Marshal(project)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func Read(path string) (*ipc.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var project ipc.Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

// NewOwner describes the current process as a session's creator.
func NewOwner(command, configPath string) *ipc.Owner {
	return &ipc.Owner{PID: os.Getpid(), Command: command, Label: os.Getenv("PROC_COMPOSE_OWNER"), Worktree: Worktree(filepath.Dir(configPath)), StartedAt: time.Now().UTC()}
}

// Worktree returns the nearest ancestor of dir that contains a .git entry (a
// repository or linked worktree), or "" when there is none.
func Worktree(dir string) string {
	for dir != "" {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}
