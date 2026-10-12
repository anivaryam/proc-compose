package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
	"github.com/spf13/cobra"
)

type explanation struct {
	Version   int             `json:"schema_version"`
	Source    string          `json:"source"`
	Project   *ipc.Project    `json:"project,omitempty"`
	Process   ipc.ProcState   `json:"process"`
	BlockedBy []ipc.ProcState `json:"blocked_by"`
	Context   logs.Page       `json:"context"`
}

func newWhyCommand(configFile *string) *cobra.Command {
	var machine bool
	var attempt string
	command := &cobra.Command{
		Use:     "why <process> | why --attempt <id|latest> [process...]",
		Short:   "Explain observed process failures and dependency blockers",
		Long:    "Explain observed process failures and dependency blockers using live state or retained journal evidence. JSON output is a bounded evidence packet, not an inferred business-level diagnosis. Run proc-compose agent-guide for the machine-readable workflow.",
		Example: "  proc-compose why api --json\n  proc-compose --session feature why worker\n  proc-compose why --attempt latest --json",
		Args: func(cmd *cobra.Command, args []string) error {
			if attempt != "" {
				return nil
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if attempt != "" {
				_, hash, _, _, err := resolveConfigPaths(*configFile)
				if err != nil {
					return err
				}
				receipt, _, err := loadAttempt(hash, attempt)
				if err != nil {
					return err
				}
				diagnosis := verify.Diagnose(receipt, classifyAttempt(receipt, hash, true), args)
				if machine {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(diagnosis)
				}
				printDiagnosis(cmd.OutOrStdout(), diagnosis)
				return nil
			}
			_, hash, socket, pidPath, err := resolveLiveDaemonPaths(*configFile)
			if err != nil {
				return err
			}
			report := explanation{Version: 1, Source: "live", BlockedBy: []ipc.ProcState{}}
			var states []ipc.ProcState
			client, dialErr := ipc.Dial(socket)
			if dialErr == nil {
				event, receiveErr := client.RecvTimeout(2 * time.Second)
				client.Close()
				if receiveErr == nil && event.Type == ipc.TypeSnapshot {
					states, report.Project = event.Processes, event.Project
				}
			}
			if states == nil {
				report.Source = "retained"
				report.Project, _ = session.Read(filepath.Join(filepath.Dir(pidPath), "pc-"+hash+".meta.json"))
				options := logs.Options{Tail: true, Limit: 1000, MaxBytes: 1 << 20, Kind: "state"}
				if report.Project != nil {
					options.RunID = report.Project.RunID
				}
				page, err := logs.Query(liveJournalPath(hash), options)
				if err != nil {
					return fmt.Errorf("no runtime evidence available: %w", err)
				}
				latest := make(map[string]ipc.ProcState)
				for _, event := range page.Events {
					if event.State != nil {
						latest[event.Process] = *event.State
					}
				}
				for _, state := range latest {
					states = append(states, state)
				}
			}
			byName := make(map[string]ipc.ProcState)
			for _, state := range states {
				byName[state.Name] = state
			}
			selected, exists := byName[args[0]]
			if !exists {
				return fmt.Errorf("no observed state for process %q in this session", args[0])
			}
			report.Process = selected
			seen := make(map[string]bool)
			var visit func(ipc.ProcState)
			visit = func(state ipc.ProcState) {
				for _, name := range state.DependsOn {
					if seen[name] {
						continue
					}
					seen[name] = true
					if dependency, ok := byName[name]; ok && !dependency.Ready {
						report.BlockedBy = append(report.BlockedBy, dependency)
						visit(dependency)
					}
				}
			}
			visit(selected)
			processes := []string{selected.Name}
			for _, state := range report.BlockedBy {
				processes = append(processes, state.Name)
			}
			report.Context, err = logs.Query(liveJournalPath(hash), logs.Options{Tail: true, Limit: 20, MaxBytes: 16 << 10, Processes: processes, RunID: runIdentity(report.Project)})
			if err != nil {
				return err
			}
			if machine {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s, ready=%v, generation=%d (%s evidence)\n", selected.Name, selected.State, selected.Ready, selected.Generation, report.Source)
			if selected.LastError != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  cause: %s\n", selected.LastError)
			}
			if selected.ExitCode != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  exit code: %d\n", *selected.ExitCode)
			}
			for _, state := range report.BlockedBy {
				fmt.Fprintf(cmd.OutOrStdout(), "  blocked by %s: %s (%s)\n", state.Name, state.State, state.LastError)
			}
			for _, event := range report.Context.Events {
				if event.Level != "debug" {
					fmt.Fprintln(cmd.OutOrStdout(), logs.Text(event))
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&machine, "json", false, "emit a bounded evidence packet for coding agents")
	command.Flags().StringVar(&attempt, "attempt", "", "explain one verification attempt (receipt id or latest) using only its own events")
	return command
}

func runIdentity(project *ipc.Project) string {
	if project == nil {
		return ""
	}
	return project.RunID
}
