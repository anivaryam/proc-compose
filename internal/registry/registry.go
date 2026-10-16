// Package registry reads the live-session registry: the metadata and PID file
// every session (foreground and daemon `up`, and `verify`) writes to the
// user's runtime directory. It only reads. It never takes a session lease or
// sends a signal, so inspecting cannot acquire cleanup authority by accident.
//
// A session's leased ports are its metadata's assigned ports: Acquire leases
// each one for the session's whole lifetime. The lease files themselves carry
// no owner and can only be observed by taking them, which the registry never
// does.
package registry

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
)

// Session is one session recorded in the runtime directory.
type Session struct {
	// Hash is the runtime hash of the session's config path and name.
	Hash    string
	Project *ipc.Project
	// Live and PID are what IsAliveFromPIDFile says about the supervisor
	// when the registry was read: its PID re-validated by start time where
	// one was recorded.
	Live bool
	PID  int
	// Record is the PID file, nil when it is missing or unreadable. Its
	// Groups are the process groups the supervisor launched (Linux and
	// macOS); daemon.StartProven(*Record) is the strict liveness check that
	// never falls back to the PID alone.
	Record *daemon.Record
}

// Name is the session's name, "default" for the unnamed one.
func (s Session) Name() string {
	if s.Project.Session == "" {
		return "default"
	}
	return s.Project.Session
}

// Discover lists every session recorded in the runtime directory, sorted by
// config path and then name. A session whose metadata cannot be read is
// skipped. Liveness is read now, so calling it again re-validates it.
func Discover() ([]Session, error) {
	metas, err := filepath.Glob(paths.Metadata("*"))
	if err != nil {
		return nil, err
	}
	sessions := []Session{}
	for _, meta := range metas {
		base := filepath.Base(meta)
		hash := strings.TrimSuffix(strings.TrimPrefix(base, "pc-"), ".meta.json")
		project, err := session.Read(meta)
		if err != nil {
			continue
		}
		s := Session{Hash: hash, Project: project}
		pidPath := paths.PID(hash)
		if alive, pid := daemon.IsAliveFromPIDFile(pidPath); alive {
			s.Live, s.PID = true, pid
		}
		if rec, err := daemon.ReadRecord(pidPath); err == nil {
			s.Record = &rec
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Project.ConfigPath != sessions[j].Project.ConfigPath {
			return sessions[i].Project.ConfigPath < sessions[j].Project.ConfigPath
		}
		return sessions[i].Name() < sessions[j].Name()
	})
	return sessions, nil
}

// PortAssignment returns the process and named port s assigned port to, or
// ok false when port is none of its named ports.
func (s Session) PortAssignment(port int) (process, name string, ok bool) {
	if port <= 0 {
		return "", "", false
	}
	processes := make([]string, 0, len(s.Project.Ports))
	for process := range s.Project.Ports {
		processes = append(processes, process)
	}
	sort.Strings(processes)
	for _, process := range processes {
		names := make([]string, 0, len(s.Project.Ports[process]))
		for name := range s.Project.Ports[process] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if s.Project.Ports[process][name] == port {
				return process, name, true
			}
		}
	}
	return "", "", false
}
