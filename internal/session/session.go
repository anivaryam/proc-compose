// Package session resolves a stack's declared endpoints and leases resources.
package session

import (
	"encoding/json"
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
		Project:   &ipc.Project{Session: name, RunID: fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()), ConfigPath: configPath, Directory: paths.SessionDir(hash), Ports: make(map[string]map[string]int), URLs: make(map[string]map[string]string), JournalPath: paths.Journal(hash)},
		variables: make(map[string]string), preferred: make(map[string]map[string]int), resources: slices.Clone(cfg.Resources), names: config.SelectedNames(cfg, filter),
	}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if err = os.MkdirAll(p.Project.Directory, 0700); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(paths.LeaseDir(), 0700); err != nil {
		return nil, err
	}
	if err = p.lease("instance:" + hash); err != nil {
		return nil, err
	}
	p.variables["session.name"] = name
	if name == "" {
		p.variables["session.name"] = "default"
	}
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
		if proc.ReadyWhen != nil {
			probe := *proc.ReadyWhen
			probe.HTTP, err = p.expand(probe.HTTP)
			if err == nil {
				probe.TCP, err = p.expand(probe.TCP)
			}
			if err != nil {
				return nil, fmt.Errorf("process %q probe: %w", name, err)
			}
			proc.ReadyWhen = &probe
		}
		result.Processes[name] = proc
	}
	return &result, nil
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

func (p *Plan) lease(label string) error {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	path := filepath.Join(paths.LeaseDir(), fmt.Sprintf("%016x.lock", h.Sum64()))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := lock(file); err != nil {
		file.Close()
		return fmt.Errorf("resource %q is owned by another session: %w", label, err)
	}
	p.leases = append(p.leases, file)
	return nil
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
