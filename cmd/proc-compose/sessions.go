package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/spf13/cobra"
)

// sessionView is a read-only description of one session for handoff.
type sessionView struct {
	Session       string                       `json:"session"`
	ConfigPath    string                       `json:"config_path"`
	RunID         string                       `json:"run_id"`
	Live          bool                         `json:"live"`
	PID           *int                         `json:"pid"`
	Retained      bool                         `json:"retained"`
	Owner         *ipc.Owner                   `json:"owner"`
	URLs          map[string]map[string]string `json:"urls"`
	LatestAttempt *attemptSummary              `json:"latest_attempt"`
	Stop          *string                      `json:"stop"`
	Inspect       string                       `json:"inspect"`
}

// shellArg quotes a value for the copy-paste commands shown to humans.
func shellArg(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`;&|<>()*?[]{}!#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func sessionFlags(project *ipc.Project) string {
	flags := "-f " + shellArg(project.ConfigPath)
	if project.Session != "" {
		flags += " --session " + shellArg(project.Session)
	}
	return flags
}

func discoverSessions() ([]sessionView, error) {
	metas, err := filepath.Glob(paths.Metadata("*"))
	if err != nil {
		return nil, err
	}
	views := []sessionView{}
	for _, meta := range metas {
		base := filepath.Base(meta)
		hash := strings.TrimSuffix(strings.TrimPrefix(base, "pc-"), ".meta.json")
		project, err := session.Read(meta)
		if err != nil {
			continue
		}
		name := project.Session
		if name == "" {
			name = "default"
		}
		view := sessionView{Session: name, ConfigPath: project.ConfigPath, RunID: project.RunID, Owner: project.Owner, URLs: project.URLs}
		if view.URLs == nil {
			view.URLs = map[string]map[string]string{}
		}
		if alive, pid := daemon.IsAliveFromPIDFile(paths.PID(hash)); alive {
			view.Live, view.PID = true, &pid
			// A live `up` session is intentionally retained until stopped;
			// a live verify session is released when its attempt finishes.
			view.Retained = project.Owner == nil || project.Owner.Command == "up"
			stop := "proc-compose " + sessionFlags(project) + " stop"
			view.Stop = &stop
		}
		view.Inspect = "proc-compose " + sessionFlags(project) + " status --json"
		// Discovery never probes the lease: a gone owner stays indeterminate.
		if rows, err := listAttempts(hash, false); err == nil && len(rows) > 0 {
			view.LatestAttempt = &rows[0]
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].ConfigPath != views[j].ConfigPath {
			return views[i].ConfigPath < views[j].ConfigPath
		}
		return views[i].Session < views[j].Session
	})
	return views, nil
}

func newSessionsCommand() *cobra.Command {
	var machine, liveOnly bool
	command := &cobra.Command{
		Use:   "sessions",
		Short: "Discover sessions, their owners, endpoints, and latest verification",
		Long: `List every session recorded in this user's runtime directory: config,
owner (command, PID, PROC_COMPOSE_OWNER label, worktree), endpoints, whether
it is live, its latest verification attempt, and how to stop it.

This command only reads metadata, PID files, and receipts; it never takes a
session lease or sends a signal, so inspecting cannot acquire cleanup
authority by accident. Sessions started with "up --silent" are retained until
stopped; verify sessions are released when their attempt finishes.`,
		Example:      "  proc-compose sessions\n  proc-compose sessions --live --json",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			views, err := discoverSessions()
			if err != nil {
				return err
			}
			if liveOnly {
				kept := views[:0]
				for _, view := range views {
					if view.Live {
						kept = append(kept, view)
					}
				}
				views = kept
			}
			if machine {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "kind": "sessions", "sessions": views})
			}
			printSessions(cmd.OutOrStdout(), views)
			return nil
		},
	}
	command.Flags().BoolVar(&machine, "json", false, "emit JSON")
	command.Flags().BoolVar(&liveOnly, "live", false, "only sessions whose owner process is running")
	return command
}

func printSessions(w io.Writer, views []sessionView) {
	if len(views) == 0 {
		fmt.Fprintln(w, "no sessions recorded")
		return
	}
	for _, v := range views {
		state := "stopped"
		if v.Live {
			state = "live pid " + strconv.Itoa(*v.PID)
			if v.Retained {
				state += " (retained)"
			}
		}
		fmt.Fprintf(w, "%s  %s  [%s]\n", v.Session, v.ConfigPath, state)
		if v.Owner != nil {
			owner := v.Owner.Command
			if v.Owner.Label != "" {
				owner += " by " + v.Owner.Label
			}
			if v.Owner.Worktree != "" {
				owner += " in " + v.Owner.Worktree
			}
			fmt.Fprintf(w, "  owner:   %s (started %s)\n", owner, v.Owner.StartedAt.Format("2006-01-02 15:04:05Z07:00"))
		}
		var urls []string
		for process, ports := range v.URLs {
			for port, url := range ports {
				urls = append(urls, process+"."+port+"="+url)
			}
		}
		sort.Strings(urls)
		if len(urls) > 0 {
			fmt.Fprintf(w, "  urls:    %s\n", strings.Join(urls, " "))
		}
		if v.LatestAttempt != nil {
			fmt.Fprintf(w, "  latest:  %s %s (%s)\n", v.LatestAttempt.AttemptID, v.LatestAttempt.Status, v.LatestAttempt.State)
		}
		if v.Stop != nil {
			fmt.Fprintf(w, "  stop:    %s\n", *v.Stop)
		}
	}
}
