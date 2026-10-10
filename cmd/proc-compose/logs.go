package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/spf13/cobra"
)

func newLogsCommand(configFile *string) *cobra.Command {
	var opts logs.Options
	var machine, follow bool
	var since string
	command := &cobra.Command{
		Use:          "logs [processes...]",
		Short:        "Query or follow bounded, structured process events",
		SilenceUsage: true,
		Long:         "Read retained events from running or stopped sessions. JSON pages include a resumable next_cursor, has_more, gap, and truncation metadata. Use --after start to page through retained history; --after CURSOR resumes without repeating records. Follow emits one JSON page per line. For the complete machine-readable contract and cleanup workflow, run proc-compose agent-guide.",
		Example:      "  proc-compose logs api --json --max-bytes 8192\n  proc-compose logs --json --after '<next_cursor>'\n  proc-compose logs worker --stream stderr --since 5m\n  proc-compose logs --follow",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Processes = append(opts.Processes, args...)
			if since != "" {
				if duration, err := time.ParseDuration(since); err == nil {
					opts.Since = time.Now().Add(-duration)
				} else {
					var err error
					opts.Since, err = time.Parse(time.RFC3339Nano, since)
					if err != nil {
						return fmt.Errorf("--since requires a duration or RFC3339 timestamp")
					}
				}
			}
			for label, value := range map[string]string{"level": opts.Level, "kind": opts.Kind, "stream": opts.Stream} {
				allowed := map[string]string{"level": " debug info warn error fatal ", "kind": " output lifecycle state session probe ", "stream": " stdout stderr supervisor "}[label]
				if value != "" && !strings.Contains(allowed, " "+value+" ") {
					return fmt.Errorf("invalid --%s %q", label, value)
				}
			}
			_, hash, _, _, err := resolveLiveDaemonPaths(*configFile)
			if err != nil {
				return err
			}
			path := liveJournalPath(hash)
			opts.Tail = opts.After == ""
			all := opts.Limit == 0
			if all {
				opts.After, opts.Tail, opts.Limit = "start", false, 1000
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			for {
				if ctx.Err() != nil {
					return nil
				}
				observed, _ := os.Stat(path)
				page, err := logs.Query(path, opts)
				if err != nil {
					if os.IsNotExist(err) {
						return fmt.Errorf("no structured journal for this session; start it with the current proc-compose (legacy text log: %s)", liveLogPath(hash))
					}
					return err
				}
				if machine {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(page); err != nil {
						return err
					}
				} else {
					for _, event := range page.Events {
						fmt.Fprintln(cmd.OutOrStdout(), logs.Text(event))
					}
					if page.Gap {
						fmt.Fprintln(cmd.ErrOrStderr(), "log cursor is outside retained history; showing the retained tail")
					}
					if page.Truncated {
						fmt.Fprintln(cmd.ErrOrStderr(), "log output bounded; use --after start to page history or increase --max-bytes")
					}
				}
				if !follow && !(all && page.HasMore) {
					return nil
				}
				opts.After, opts.Tail = page.NextCursor, false
				if opts.After == "" {
					opts.After = "start"
				}
				if page.HasMore {
					continue
				}
				if err := waitLogChange(ctx, path, observed); err != nil {
					return nil
				}
			}
		},
	}
	command.Flags().IntVarP(&opts.Limit, "tail", "n", 50, "maximum events per page (0 prints retained history in bounded pages)")
	command.Flags().IntVar(&opts.MaxBytes, "max-bytes", 64<<10, "maximum encoded event bytes per page (1024..1048576)")
	command.Flags().StringVar(&opts.After, "after", "", "resume after an event cursor, or start at oldest retained history")
	command.Flags().StringSliceVar(&opts.Processes, "process", nil, "filter process names (comma-separated or repeated)")
	command.Flags().StringVar(&opts.Level, "level", "", "filter exact severity: debug, info, warn, error, fatal")
	command.Flags().StringVar(&opts.Kind, "kind", "", "filter output, lifecycle, state, session, or probe")
	command.Flags().StringVar(&opts.Code, "code", "", "filter a stable supervisor event code")
	command.Flags().StringVar(&opts.Stream, "stream", "", "filter stdout, stderr, or supervisor")
	command.Flags().StringVar(&opts.Search, "search", "", "case-insensitive message substring")
	command.Flags().StringVar(&since, "since", "", "duration such as 5m, or RFC3339 timestamp")
	command.Flags().StringVar(&opts.RunID, "run", "", "filter daemon run identity")
	command.Flags().Uint64Var(&opts.Generation, "generation", 0, "filter a process incarnation")
	command.Flags().BoolVar(&machine, "json", false, "emit bounded JSON pages with cursor metadata")
	command.Flags().BoolVarP(&follow, "follow", "F", false, "follow new events until interrupted")
	return command
}

func liveJournalPath(hash string) string {
	pidPath, _ := liveDaemonPaths(hash)
	if project, err := session.Read(filepath.Join(filepath.Dir(pidPath), "pc-"+hash+".meta.json")); err == nil && project.JournalPath != "" {
		return project.JournalPath
	}
	for _, directory := range candidateRuntimeDirs() {
		path := filepath.Join(directory, "pc-"+hash+".events.jsonl")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(filepath.Dir(pidPath), "pc-"+hash+".events.jsonl")
}

func waitLogChange(ctx context.Context, path string, initial os.FileInfo) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			current, _ := os.Stat(path)
			if initial == nil || current == nil || !os.SameFile(initial, current) || initial.Size() != current.Size() || !initial.ModTime().Equal(current.ModTime()) {
				return nil
			}
		}
	}
}
