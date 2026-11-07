// Package jobs retains and controls detached development checks. Execution and
// cleanup remain owned by verify; a job is a durable handle, never another runner.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/sharedfile"
	"github.com/anivaryam/proc-compose/internal/verify"
)

const maxRecordBytes = 128 << 10

// Request contains only execution inputs. Inherited environment values are
// passed to the detached process, not copied into persistent metadata.
type Request struct {
	TaskName             string        `json:"task_name,omitempty"`
	CheckTimeoutExplicit bool          `json:"check_timeout_explicit,omitempty"`
	ConfigPath           string        `json:"config_path"`
	Session              string        `json:"session"`
	Standalone           bool          `json:"standalone"`
	Selectors            []string      `json:"selectors,omitempty"`
	Argv                 []string      `json:"argv"`
	Dir                  string        `json:"dir,omitempty"`
	StartupTimeout       time.Duration `json:"startup_timeout_ns"`
	CheckTimeout         time.Duration `json:"check_timeout_ns"`
	CleanupTimeout       time.Duration `json:"cleanup_timeout_ns"`
	ToolVersion          string        `json:"tool_version"`
	RequireEnvironment   bool          `json:"require_environment,omitempty"`
	RefuseCrossSession   bool          `json:"refuse_cross_session,omitempty"`
}

type Record struct {
	Version   int            `json:"schema_version"`
	ID        string         `json:"job_id"`
	State     string         `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	OwnerPID  int            `json:"owner_pid,omitempty"` // informational; never signalling authority
	Request   Request        `json:"request"`
	Result    *verify.Result `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type View struct {
	TaskName        string         `json:"task_name,omitempty"`
	Version         int            `json:"schema_version"`
	Kind            string         `json:"kind"`
	ID              string         `json:"job_id"`
	State           string         `json:"state"`
	Terminal        bool           `json:"terminal"`
	CreatedAt       time.Time      `json:"created_at"`
	OwnerPID        int            `json:"owner_pid,omitempty"`
	ConfigPath      string         `json:"config_path"`
	Session         string         `json:"session"`
	Directory       string         `json:"directory"`
	Result          *verify.Result `json:"result,omitempty"`
	Message         string         `json:"message,omitempty"`
	WaitTimedOut    bool           `json:"wait_timed_out,omitempty"`
	CancelRequested bool           `json:"cancel_requested,omitempty"`
}

type Page struct {
	Version int    `json:"schema_version"`
	Jobs    []View `json:"jobs"`
	Before  string `json:"next_before,omitempty"`
	HasMore bool   `json:"has_more"`
}

type Store struct{ Root string }

func Open() (*Store, error) {
	root, err := paths.Jobs()
	if err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

func (s *Store) directory(id string) (string, error) {
	if !filepath.IsAbs(s.Root) || !verify.ValidAttemptID(id) {
		return "", errors.New("invalid job store or job ID")
	}
	return filepath.Join(s.Root, id), nil
}

func save(path string, record *Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return errors.New("job record exceeds 128 KiB")
	}
	return daemon.WriteFileAtomic(path, data, 0600)
}

func (s *Store) read(id string) (*Record, string, error) {
	dir, err := s.directory(id)
	if err != nil {
		return nil, "", err
	}
	f, err := sharedfile.Open(filepath.Join(dir, "job.json"))
	if err != nil {
		return nil, dir, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil || len(data) > maxRecordBytes {
		return nil, dir, errors.New("cannot read bounded job record")
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, dir, err
	}
	if rec.Version != 1 || rec.ID != id {
		return nil, dir, errors.New("invalid job record identity or version")
	}
	switch rec.State {
	case "queued", "running", "completed", "failed":
	default:
		return nil, dir, errors.New("invalid job state")
	}
	if rec.State == "completed" && !verify.ValidTerminalResult(rec.Result) {
		return nil, dir, errors.New("job has no terminal result")
	}
	if rec.State == "completed" {
		var present struct {
			Result struct {
				ExitCode *int `json:"exit_code"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &present); err != nil || present.Result.ExitCode == nil {
			return nil, dir, errors.New("job result has no exit code")
		}
	}
	return &rec, dir, nil
}

// Prepare records a unique request before a worker can be launched. A default
// job-specific session lets independent jobs coexist without touching a stack.
func (s *Store) Prepare(opts verify.Options) (*View, error) {
	_, size := verify.ArgvDigest(opts.Argv)
	if (opts.TaskName == "" && (len(opts.Argv) == 0 || opts.Argv[0] == "")) || size > verify.MaxArgvBytes || !filepath.IsAbs(opts.ConfigPath) {
		return nil, errors.New("job needs an absolute config context and bounded command argv")
	}
	id, err := verify.NewAttemptID(time.Now())
	if err != nil {
		return nil, err
	}
	dir, err := s.directory(id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if opts.Session == "" {
		opts.Session = "job-" + id
	}
	rec := &Record{Version: 1, ID: id, State: "queued", CreatedAt: time.Now().UTC(), Request: Request{
		TaskName: opts.TaskName, CheckTimeoutExplicit: opts.CheckTimeoutExplicit,
		ConfigPath: opts.ConfigPath, Session: opts.Session, Standalone: opts.Standalone,
		Selectors: opts.Selectors, Argv: opts.Argv, Dir: opts.Dir,
		StartupTimeout: opts.StartupTimeout, CheckTimeout: opts.CheckTimeout, CleanupTimeout: opts.CleanupTimeout,
		ToolVersion: opts.ToolVersion, RequireEnvironment: opts.RequireEnvironment, RefuseCrossSession: opts.RefuseCrossSession,
	}}
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *Store) Get(id string) (*View, error) {
	rec, dir, err := s.read(id)
	if err != nil {
		return nil, err
	}
	v := &View{Version: 1, Kind: "job.response", ID: id, State: rec.State, CreatedAt: rec.CreatedAt,
		TaskName: rec.Request.TaskName,
		OwnerPID: rec.OwnerPID, ConfigPath: rec.Request.ConfigPath, Session: rec.Request.Session,
		Directory: dir, Result: rec.Result, Message: rec.Error}
	if _, err := os.Stat(filepath.Join(dir, "cancel")); err == nil {
		v.CancelRequested = true
	}
	switch rec.State {
	case "completed":
		v.State, v.Terminal = rec.Result.Status, true
	case "failed":
		v.Terminal = true
	case "running":
		held, err := session.FileLeaseHeld(filepath.Join(dir, "worker.lock"))
		if err != nil {
			v.State, v.Message = "indeterminate", "worker lease could not be observed"
		} else if !held {
			// Completion may have been persisted after our first read and
			// before lease release. Re-read after proven release.
			latest, _, err := s.read(id)
			if err != nil {
				v.State, v.Message = "indeterminate", "terminal job record could not be read"
				return v, nil
			}
			if latest.State != "running" {
				return s.Get(id)
			}
			v.State, v.Message = "abandoned", "worker released its lease without a terminal result; cleanup is unverified"
		}
	}
	return v, nil
}

func (s *Store) List(limit int, before string) (*Page, error) {
	if limit < 1 || limit > 100 || (before != "" && !verify.ValidAttemptID(before)) {
		return nil, errors.New("job pages require 1..100 entries and a valid before ID")
	}
	page := &Page{Version: 1, Jobs: []View{}}
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !verify.ValidAttemptID(id) || (before != "" && id >= before) {
			continue
		}
		v, err := s.Get(id)
		if err != nil {
			continue // malformed files never become a successful job
		}
		if len(page.Jobs) == limit {
			page.HasMore = true
			break
		}
		v.Result = nil // list summaries cannot multiply 64 KiB result packets
		page.Jobs = append(page.Jobs, *v)
		page.Before = id
	}
	return page, nil
}

func (s *Store) Logs(id string, opts logs.Options) (logs.Page, error) {
	_, dir, err := s.read(id)
	if err != nil {
		return logs.Page{}, err
	}
	page, err := logs.Query(filepath.Join(dir, "events.jsonl"), opts)
	if errors.Is(err, os.ErrNotExist) {
		return logs.Page{Version: 1, Events: page.Events, NextCursor: opts.After}, nil
	}
	return page, err
}

// Cancel requests cancellation through this immutable job's mailbox. Only its
// worker consumes the request; no stored PID or session name is signalled.
func (s *Store) Cancel(id string) (*View, error) {
	v, err := s.Get(id)
	if err != nil || v.Terminal {
		return v, err
	}
	if v.State == "abandoned" || v.State == "indeterminate" {
		return v, errors.New(v.Message)
	}
	if err := daemon.WriteFileAtomic(filepath.Join(v.Directory, "cancel"), []byte("cancel\n"), 0600); err != nil {
		return v, err
	}
	v.CancelRequested = true
	return v, nil
}

func (s *Store) Wait(ctx context.Context, id string) (*View, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		v, err := s.Get(id)
		if err != nil || v.Terminal || v.State == "abandoned" || v.State == "indeterminate" {
			return v, err
		}
		select {
		case <-ctx.Done():
			v.WaitTimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			return v, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Launch waits only for the worker's durable acknowledgement, not completion.
// If the handoff is interrupted, a mailbox request prevents a late worker from
// starting work. The returned view always names any job that was created.
func (s *Store) Launch(ctx context.Context, opts verify.Options) (*View, error) {
	v, err := s.Prepare(opts)
	if err != nil {
		return nil, err
	}
	if _, err := daemon.Reexec([]string{"jobs", "_worker", "--root", s.Root, v.ID}, filepath.Join(v.Directory, "worker.log")); err != nil {
		// Start failed: no child can claim the request later.
		if rec, _, readErr := s.read(v.ID); readErr == nil {
			rec.State, rec.Error = "failed", err.Error()
			if saveErr := save(filepath.Join(v.Directory, "job.json"), rec); saveErr == nil {
				v, _ = s.Get(v.ID)
			}
		}
		return v, err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := s.Get(v.ID)
		if err != nil {
			_, _ = s.Cancel(v.ID)
			return v, err
		}
		if current.State != "queued" {
			if current.State == "abandoned" || current.State == "indeterminate" {
				return current, errors.New(current.Message)
			}
			return current, nil
		}
		select {
		case <-ctx.Done():
			_, _ = s.Cancel(v.ID)
			v.CancelRequested = true
			return v, fmt.Errorf("job launch acknowledgement: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Work claims a job once. The lease is persistent and independent of session
// runtime files, so loss of XDG_RUNTIME_DIR cannot turn a live worker into an
// abandoned job. Terminal persistence happens before releasing this lease.
func (s *Store) Work(ctx context.Context, id string) error {
	dir, err := s.directory(id)
	if err != nil {
		return err
	}
	lease, err := session.LeaseFile(filepath.Join(dir, "worker.lock"))
	if err != nil {
		return err
	}
	defer lease.Close()
	rec, _, err := s.read(id)
	if err != nil {
		return err
	}
	if rec.State != "queued" {
		return errors.New("job was already claimed; jobs are never replayed")
	}
	rec.State, rec.OwnerPID = "running", os.Getpid()
	if err := save(filepath.Join(dir, "job.json"), rec); err != nil {
		return err
	}
	request := rec.Request
	hash, err := paths.FromConfig(request.ConfigPath, request.Session)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	checkCancel := func() {
		if _, err := os.Stat(filepath.Join(dir, "cancel")); !errors.Is(err, os.ErrNotExist) {
			cancel() // unreadable cancellation state must not permit more work
		}
	}
	checkCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCancel()
			}
		}
	}()
	result := verify.Run(ctx, verify.Options{
		TaskName: request.TaskName, CheckTimeoutExplicit: request.CheckTimeoutExplicit,
		ConfigPath: request.ConfigPath, Session: request.Session, Hash: hash, Standalone: request.Standalone,
		Selectors: request.Selectors, Argv: request.Argv, Dir: request.Dir, EvidenceDir: dir,
		StartupTimeout: request.StartupTimeout, CheckTimeout: request.CheckTimeout, CleanupTimeout: request.CleanupTimeout,
		ToolVersion: request.ToolVersion, RequireEnvironment: request.RequireEnvironment, RefuseCrossSession: request.RefuseCrossSession,
		Log: os.Stderr,
	})
	cancel()
	<-done
	rec.State, rec.Result = "completed", result
	if _, compact, err := verify.Encode(result); err != nil || compact {
		rec.State, rec.Result, rec.Error = "failed", nil, "result_size_limit: verification result could not be retained"
	}
	return save(filepath.Join(dir, "job.json"), rec)
}
