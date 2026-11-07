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
	"sync"
	"time"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/sharedfile"
)

type Plan struct {
	Project   *ipc.Project
	Config    *config.Config
	leases    []*os.File
	variables map[string]string
	preferred map[string]map[string]int
	resources []string
	names     []string
	mergePort int // the merge proxy port the plan leases and holds; 0 when it leases none

	// portsMu guards held and uses: the runner releases ports from its launch
	// goroutines while a reload may resolve.
	portsMu sync.Mutex
	// held keeps each allocated port, and the merge port, bound by port
	// number until the first process that uses it is about to start
	// (ReleasePorts) or the plan closes.
	held map[int]net.Listener
	// uses lists, per process, the held ports it declares or references; the
	// merge proxy uses the merge port.
	uses map[string][]int
}

func Acquire(cfg *config.Config, configPath, hash, name string, filter []string) (_ *Plan, err error) {
	// The filter is validated before anything is leased: an unknown or
	// teardown name is refused outright instead of silently selecting
	// nothing.
	for _, selector := range filter {
		if selectorErr := config.CheckSelector(cfg, selector); selectorErr != nil {
			return nil, selectorErr
		}
	}
	p := &Plan{
		Project:   &ipc.Project{Session: name, RunID: ipc.NewRunID(), ConfigPath: configPath, Directory: paths.SessionDir(hash), Ports: make(map[string]map[string]int), URLs: make(map[string]map[string]string), JournalPath: paths.Journal(hash)},
		variables: make(map[string]string), preferred: make(map[string]map[string]int), resources: slices.Clone(cfg.Resources),
		held: make(map[int]net.Listener), uses: make(map[string][]int),
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
	leased := make(map[string]bool, len(resources))
	for _, label := range resources {
		resolved, expandErr := p.expand(label)
		if expandErr != nil {
			return nil, expandErr
		}
		resolved = resourceIdentity(resolved)
		if leased[resolved] {
			// Two labels expanded to the same resource: the plan already
			// holds that lease, so it is not taken twice (the first
			// occurrence wins).
			continue
		}
		leased[resolved] = true
		if err = p.lease("resource:" + resolved); err != nil {
			return nil, err
		}
		p.Project.Resources = append(p.Project.Resources, resolved)
	}
	// The merge proxy listens on a fixed port baked into its command, so the
	// port is leased and held at that number rather than allocated: it is
	// never moved and never recorded as a named port, and a busy merge port
	// refuses the start. It is held before the named ports are assigned, so a
	// named port that prefers it is the one moved.
	//
	// The port is taken only when the proxy is part of the selected process
	// set. A filtered start that does not select the proxy never binds the
	// port, so holding it would refuse a second session that would have been
	// able to bind it.
	if cfg.Merge != nil && cfg.Merge.Port > 0 && slices.Contains(p.names, config.MergePortProcessName) {
		if err = p.hold(cfg.Merge.Port); err != nil {
			return nil, fmt.Errorf("merge.port %d: %w", cfg.Merge.Port, err)
		}
		p.mergePort = cfg.Merge.Port
	}
	// A named port a merge upstream is started on is assigned its preferred
	// port or refuses the start: the proxy and the upstream's injected
	// readiness probe target the merge port it was matched at, so a moved
	// port would leave the upstream listening where nothing looks. These
	// ports are assigned first, so a port that merely prefers the same number
	// is the one moved.
	upstreamPorts := config.MergeUpstreamPorts(cfg)
	type endpoint struct {
		process, portName string
		pinned            bool
	}
	var pinned, movable []endpoint
	for _, process := range p.names {
		declared := cfg.Processes[process].Ports
		p.preferred[process] = maps.Clone(declared)
		portNames := make([]string, 0, len(declared))
		for portName := range declared {
			portNames = append(portNames, portName)
		}
		sort.Strings(portNames)
		for _, portName := range portNames {
			if slices.Contains(upstreamPorts, process+"."+portName) {
				pinned = append(pinned, endpoint{process, portName, true})
			} else {
				movable = append(movable, endpoint{process, portName, false})
			}
		}
	}
	for _, e := range append(pinned, movable...) {
		process, portName := e.process, e.portName
		preferred := cfg.Processes[process].Ports[portName]
		var port int
		var allocateErr error
		if e.pinned {
			if allocateErr = p.hold(preferred); allocateErr != nil {
				allocateErr = fmt.Errorf("port %d is a merge upstream's port, which is never moved: %w", preferred, allocateErr)
			}
			port = preferred
		} else {
			port, allocateErr = p.allocate(preferred)
		}
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
	p.Config, err = p.Resolve(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Resolve also applies reloads against the already-owned endpoint assignments.
// Changing leases requires a new session, rather than silently changing owners.
func (p *Plan) Resolve(cfg *config.Config) (*config.Config, error) {
	if !sameResources(p.resources, cfg.Resources) {
		return nil, fmt.Errorf("resources changed; restart the session")
	}
	// The leased merge port is baked into the proxy's command, so a reload
	// that moves it would restart the proxy on a port the session does not
	// lease. A reload that drops the merge section leaves the running proxy
	// alone (hot-remove is not supported) and is not a change of lease.
	if p.mergePort != 0 && cfg.Merge != nil && cfg.Merge.Port != p.mergePort {
		return nil, fmt.Errorf("merge port changed; restart the session")
	}
	result := *cfg
	result.Processes = maps.Clone(cfg.Processes)
	uses := make(map[string][]int, len(p.names))
	for _, name := range p.names {
		proc, exists := cfg.Processes[name]
		if !exists {
			continue
		}
		if !maps.Equal(proc.Ports, p.preferred[name]) {
			return nil, fmt.Errorf("named ports for %q changed; restart the session", name)
		}
		// A process uses the ports it declares and every port its expanded
		// fields reference: any of them may be the one that binds the port.
		// The merge proxy binds the merge port.
		used := slices.Collect(maps.Values(p.Project.Ports[name]))
		if name == config.MergePortProcessName && p.mergePort != 0 {
			used = append(used, p.mergePort)
		}
		expand := func(value string) (string, error) {
			used = append(used, p.referencedPorts(value)...)
			return p.expand(value)
		}
		var err error
		proc.Cmd, err = expand(proc.Cmd)
		if err != nil {
			return nil, fmt.Errorf("process %q cmd: %w", name, err)
		}
		proc.Env = maps.Clone(proc.Env)
		if proc.Env == nil {
			proc.Env = make(map[string]string)
		}
		for key, value := range proc.Env {
			proc.Env[key], err = expand(value)
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
		proc.Dir, err = expand(proc.Dir)
		if err != nil {
			return nil, fmt.Errorf("process %q dir: %w", name, err)
		}
		if proc.Dir == "" && p.Project.Session != "" {
			proc.Dir = filepath.Dir(p.Project.ConfigPath)
		}
		if proc.Dir != "" && !filepath.IsAbs(proc.Dir) {
			proc.Dir = filepath.Join(filepath.Dir(p.Project.ConfigPath), proc.Dir)
		}
		if proc.ReadyWhen, err = expandProbe(proc.ReadyWhen, expand); err != nil {
			return nil, fmt.Errorf("process %q probe: %w", name, err)
		}
		if proc.HealthWhen, err = expandProbe(proc.HealthWhen, expand); err != nil {
			return nil, fmt.Errorf("process %q health_when: %w", name, err)
		}
		result.Processes[name] = proc
		uses[name] = used
	}
	// Uses only grow: the runner can still refuse a reload this resolved, and
	// releasing a port early only shortens its hold.
	p.portsMu.Lock()
	for name, used := range uses {
		merged := append(p.uses[name], used...)
		slices.Sort(merged)
		p.uses[name] = slices.Compact(merged)
	}
	p.portsMu.Unlock()
	return &result, nil
}

// referencedPorts returns the allocated ports value names through a
// ${ports.*} or ${urls.*} reference.
func (p *Plan) referencedPorts(value string) []int {
	var ports []int
	for process, named := range p.Project.Ports {
		for portName, port := range named {
			ref := process + "." + portName + "}"
			if strings.Contains(value, "${ports."+ref) || strings.Contains(value, "${urls."+ref) {
				ports = append(ports, port)
			}
		}
	}
	return ports
}

// sameResources reports whether two resource label lists name the same set of
// resources. The order of the labels is not significant: the leases are taken
// over the sorted labels, so a reordering changes no lease and is not a
// resource-set change.
func sameResources(a, b []string) bool {
	sorted := func(labels []string) []string {
		out := slices.Clone(labels)
		sort.Strings(out)
		return out
	}
	return slices.Equal(sorted(a), sorted(b))
}

// resourceIdentity returns the lease identity of an expanded resource label. A
// label that is an absolute file path once expanded is cleaned, so the
// spellings of one path are one resource: `${config.dir}/db` and the literal
// path, a doubled or trailing separator, and on Windows `/` against `\`. Any
// other label — `db`, `tcp:127.0.0.1:5432`, a URL, a relative path, or a path
// behind a prefix such as `next-build:` — is compared as written.
func resourceIdentity(label string) string {
	if filepath.IsAbs(label) {
		return filepath.Clean(label)
	}
	return label
}

// expandProbe returns a copy of probe with its endpoint references resolved by
// expand, or nil for no probe. A log probe is a regex and is left as written.
func expandProbe(probe *config.ReadyWhen, expand func(string) (string, error)) (*config.ReadyWhen, error) {
	if probe == nil {
		return nil, nil
	}
	expanded := *probe
	var err error
	for _, field := range []*string{&expanded.HTTP, &expanded.TCP, &expanded.Cmd} {
		if *field, err = expand(*field); err != nil {
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
	// Bound reports a port the session holds at its exact number (hold) that
	// it leased but could not bind: a program outside proc-compose's leases
	// is using it, rather than another session.
	Bound bool
}

func (e *LeaseError) Error() string {
	if e.Bound {
		return fmt.Sprintf("resource %q is in use by another program: %v", e.Label, e.Err)
	}
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
		if err = p.lease(fmt.Sprintf("tcp:127.0.0.1:%d", port)); err != nil {
			listener.Close()
			continue
		}
		// The listener stays open: the lease only excludes other sessions,
		// while the bound port excludes every program until the process
		// that uses it is about to start.
		p.portsMu.Lock()
		p.held[port] = listener
		p.portsMu.Unlock()
		return port, nil
	}
	return 0, fmt.Errorf("no available named port starting at %d", preferred)
}

// hold leases port and keeps it bound, like allocate, but at that exact
// number: for a port baked into the merge wiring, which the session cannot
// move. The lease is taken first, so another session's port is reported as
// that session's. A port the session could lease but not bind is a Bound
// *LeaseError. A port the plan already holds — the merge port named as an
// upstream's port too — is a configuration error, not another owner.
func (p *Plan) hold(port int) error {
	label := fmt.Sprintf("tcp:127.0.0.1:%d", port)
	p.portsMu.Lock()
	_, own := p.held[port]
	p.portsMu.Unlock()
	if own {
		return fmt.Errorf("port %d is already held by this session for another use", port)
	}
	if err := p.lease(label); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return &LeaseError{Label: label, Err: err, Bound: true}
	}
	p.portsMu.Lock()
	p.held[port] = listener
	p.portsMu.Unlock()
	return nil
}

// ReleasePorts closes the listeners that hold the allocated ports process name
// declares or references, and for the merge proxy the merge port, so that it
// can bind them. The runner calls it right
// before each launch (runner.Runner.BeforeStart), which narrows the window in
// which another program can take a leased port to the launch itself. The
// leases stay held. A port already released, a later launch and a name that
// uses no held port are no-ops.
func (p *Plan) ReleasePorts(name string) {
	p.portsMu.Lock()
	defer p.portsMu.Unlock()
	for _, port := range p.uses[name] {
		if listener, ok := p.held[port]; ok {
			_ = listener.Close()
			delete(p.held, port)
		}
	}
}

// Close releases the ports still held, then the leases.
func (p *Plan) Close() {
	p.portsMu.Lock()
	for port, listener := range p.held {
		_ = listener.Close()
		delete(p.held, port)
	}
	p.portsMu.Unlock()
	for i := len(p.leases) - 1; i >= 0; i-- {
		_ = p.leases[i].Close()
	}
	p.leases = nil
}

// Save writes the session metadata atomically: a reader never observes a
// truncated document.
func Save(path string, project *ipc.Project) error {
	data, err := json.Marshal(project)
	if err != nil {
		return err
	}
	return daemon.WriteFileAtomic(path, data, 0600)
}

// Read reads the session metadata. It reads through sharedfile, so on Windows
// a Save that replaces the file while it is read still succeeds.
func Read(path string) (*ipc.Project, error) {
	data, err := sharedfile.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var project ipc.Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, err
	}
	if project.RunID == "" && project.ConfigPath == "" {
		// A document that describes no session is not a session: a null or
		// empty object would otherwise register a phantom session.
		return nil, fmt.Errorf("invalid session metadata %s: the document describes no session", path)
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
