package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anivaryam/proc-compose/internal/daemon"
	"github.com/anivaryam/proc-compose/internal/paths"
	"github.com/anivaryam/proc-compose/internal/session"
	"github.com/anivaryam/proc-compose/internal/verify"
	"github.com/spf13/cobra"
)

// attemptSummary is one row of the attempts listing.
type attemptSummary struct {
	AttemptID     string   `json:"attempt_id"`
	State         string   `json:"state"`
	RecordedState string   `json:"recorded_state"`
	Status        string   `json:"status"`
	ExitCode      int      `json:"exit_code"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    *string  `json:"finished_at"`
	Argv          []string `json:"argv"`
	ReceiptPath   string   `json:"receipt_path"`
	CleanupStatus string   `json:"cleanup_status"`
}

// instanceHeld probes the session lease. It is a variable so tests can prove
// which paths never reach it.
var instanceHeld = session.InstanceHeld

// classifyAttempt reports a receipt's state from positive evidence. Active
// needs the session PID file to still name the live owner. The lease
// is probed only for explicit inspection (probeLease) and, inside Classify,
// only once the recorded owner is positively gone, so inspecting a live or
// unknown owner never touches its lease. Without the probe, a gone owner is
// indeterminate rather than abandoned.
func classifyAttempt(receipt *verify.Result, hash string, probeLease bool) string {
	hostname, _ := os.Hostname()
	evidence := verify.OwnerEvidence{
		OwnerAlive: func(owner verify.Owner) (bool, bool) {
			if owner.Hostname != hostname || owner.PID <= 0 {
				return false, false
			}
			return daemon.IsAlive(owner.PID), true
		},
		OwnerRunning: func(owner verify.Owner) bool {
			alive, pid := daemon.IsAliveFromPIDFile(paths.PID(hash))
			return alive && pid == owner.PID
		},
	}
	if probeLease {
		evidence.LeaseHeld = func() (bool, error) { return instanceHeld(hash) }
	}
	return verify.Classify(receipt, evidence)
}

func loadAttempt(hash, id string) (*verify.Result, string, error) {
	dir := paths.SessionDir(hash)
	if id == "" || id == "latest" {
		receipts, err := verify.ListReceipts(dir)
		if err != nil {
			return nil, "", err
		}
		if len(receipts) == 0 {
			return nil, "", fmt.Errorf("no verification receipts for this session (receipts live in the runtime directory and may be lost at logout/reboot)")
		}
		id = strings.TrimSuffix(filepath.Base(receipts[0]), ".json")
	}
	if strings.ContainsAny(id, `/\.`) {
		return nil, "", fmt.Errorf("invalid attempt id %q", id)
	}
	path := filepath.Join(verify.AttemptsDir(dir), id+".json")
	receipt, err := verify.ReadReceipt(path)
	if err != nil {
		return nil, "", fmt.Errorf("attempt %s: %w", id, err)
	}
	return receipt, path, nil
}

func newAttemptsCommand(configFile *string) *cobra.Command {
	var machine bool
	command := &cobra.Command{
		Use:   "attempts [attempt-id|latest]",
		Short: "List or show verification receipts for this session",
		Long: `List verification attempts recorded for the selected config/session, newest
first, or print one receipt. A nonterminal receipt is classified active only
while its owner process is alive and still runs the session; abandoned requires positive evidence that
the owner is gone and the session lease is free; anything else is
indeterminate. Receipts live in the runtime directory and are not archival.`,
		Example:      "  proc-compose attempts\n  proc-compose --session checkout-fix attempts latest --json",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, hash, _, _, err := resolveConfigPaths(*configFile)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				receipt, _, err := loadAttempt(hash, args[0])
				if err != nil {
					return err
				}
				receipt.State = classifyAttempt(receipt, hash, true)
				if machine {
					return json.NewEncoder(out).Encode(receipt)
				}
				printVerifyHuman(out, receipt)
				fmt.Fprintf(out, "  state:    %s\n", receipt.State)
				return nil
			}
			rows, err := listAttempts(hash, true)
			if err != nil {
				return err
			}
			if machine {
				return json.NewEncoder(out).Encode(map[string]any{"schema_version": 1, "kind": "verify.attempts", "attempts": rows, "retention": verify.Retention})
			}
			printAttempts(out, rows)
			return nil
		},
	}
	command.Flags().BoolVar(&machine, "json", false, "emit JSON")
	return command
}

func listAttempts(hash string, probeLease bool) ([]attemptSummary, error) {
	receipts, err := verify.ListReceipts(paths.SessionDir(hash))
	if err != nil {
		return nil, err
	}
	rows := []attemptSummary{}
	for _, path := range receipts {
		receipt, err := verify.ReadReceipt(path)
		if err != nil {
			continue
		}
		row := attemptSummary{State: classifyAttempt(receipt, hash, probeLease), RecordedState: receipt.State, Status: receipt.Status, ExitCode: receipt.ExitCode, StartedAt: receipt.StartedAt, FinishedAt: receipt.FinishedAt, Argv: receipt.Check.Argv, ReceiptPath: path, CleanupStatus: receipt.Cleanup.Status}
		if receipt.AttemptID != nil {
			row.AttemptID = *receipt.AttemptID
		}
		if row.State != verify.StateCompleted {
			// A nonterminal record never carries a final verdict.
			row.Status = "unknown"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func printAttempts(w io.Writer, rows []attemptSummary) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no verification attempts recorded for this session")
		return
	}
	for _, row := range rows {
		fmt.Fprintf(w, "%s  %-13s %-11s cleanup=%-10s %s  %s\n", row.AttemptID, row.State, row.Status, row.CleanupStatus, row.StartedAt, strings.Join(row.Argv, " "))
	}
}

func printDiagnosis(w io.Writer, d verify.Diagnosis) {
	fmt.Fprintf(w, "attempt %s: %s (%s); boundary=%s\n", d.AttemptID, d.Status, d.AttemptState, d.Boundary)
	fmt.Fprintf(w, "  phases: validation=%s startup=%s check=%s cleanup=%s\n", d.Phases["validation"], d.Phases["startup"], d.Phases["check"], d.Phases["cleanup"])
	for _, e := range d.Errors {
		fmt.Fprintf(w, "  error: %s ×%d\n", e.Code, e.Count)
	}
	for _, p := range d.Processes {
		fmt.Fprintf(w, "  %s generations=%v\n", p.Name, p.Generations)
		for _, t := range p.Transitions {
			line := fmt.Sprintf("    [gen %d] %s ready=%t", t.Generation, t.State, t.Ready)
			if t.LastError != "" {
				line += " — " + t.LastError
			}
			fmt.Fprintln(w, line)
		}
		for _, o := range p.Output {
			count := ""
			if o.Count > 1 {
				count = fmt.Sprintf(" (×%d)", o.Count)
			}
			fmt.Fprintf(w, "    %s%s\n", o.Message, count)
		}
	}
	if d.HistoryMissing || d.Gap || d.Truncated {
		fmt.Fprintf(w, "  evidence incomplete: missing=%t gap=%t truncated=%t\n", d.HistoryMissing, d.Gap, d.Truncated)
	}
	fmt.Fprintf(w, "  limitations: %s\n", strings.Join(d.Limitations, ", "))
}
