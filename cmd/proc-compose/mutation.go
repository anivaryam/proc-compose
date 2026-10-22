package main

import (
	"fmt"
	"strings"

	"github.com/anivaryam/proc-compose/internal/ipc"
	"github.com/anivaryam/proc-compose/internal/verify"
	"github.com/spf13/cobra"
)

// mutationResponse reuses the daemon's result fields, but Status describes the
// whole CLI operation: an accepted restart can still fail its readiness wait.
type mutationResponse struct {
	Operation string `json:"operation"`
	Process   string `json:"process,omitempty"`
	ipc.CommandResult
	Ready *bool `json:"ready,omitempty"`
}

func (r *mutationResponse) acknowledge(event ipc.Event) {
	r.CommandResult = ipc.CommandResult{
		Status:     event.Ack,
		Message:    event.AckDetail,
		Generation: event.AckGeneration,
	}
}

func addMutationJSON(command *cobra.Command, machine *bool, response *mutationResponse) {
	command.Flags().BoolVar(machine, "json", false, "emit one JSON result after flag parsing")
	validateArgs, run := command.Args, command.RunE
	command.Args = func(cmd *cobra.Command, args []string) error {
		var err error
		if validateArgs != nil {
			err = validateArgs(cmd, args)
		}
		if !*machine {
			return err
		}
		cmd.SilenceErrors = true
		// An invalid --session is refused after this, by the root pre-run
		// hook (refuseSession), as on every other command.
		if err != nil {
			return response.render(cmd, err, 1)
		}
		return nil
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if !*machine {
			return err
		}
		return response.render(cmd, err, 1)
	}
}

// render writes r as the command's one JSON result, recording err as its
// failure, and returns the exit status: failureExit when err is not nil.
func (r *mutationResponse) render(cmd *cobra.Command, err error, failureExit int) error {
	if err != nil {
		if r.Status != "busy" {
			r.Status = "error"
		}
		r.Message = err.Error()
	}
	if writeErr := writeConfigJSON(cmd.OutOrStdout(), r); writeErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: cannot write %s result: %v\n", r.Operation, writeErr)
		return exitCodeError{1}
	}
	if err != nil {
		return exitCodeError{failureExit}
	}
	return nil
}

// refuseSession reports an invalid --session from the root pre-run hook,
// before the command has done anything. Every command refuses it the same way
// with exit status 2, the usage status verify and compare document; those two
// render their own structured refusal and never reach this.
//
// With --json, stdout carries one result object in the form restart and
// reload use for their errors, {operation, status: "error", message}, and
// stderr stays empty. Without it, the error is printed on stderr.
func refuseSession(cmd *cobra.Command, err error) error {
	cmd.SilenceErrors = true
	if !jsonRequested(cmd) {
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
		return exitCodeError{verify.ExitUsage}
	}
	response := mutationResponse{Operation: strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")}
	return response.render(cmd, err, verify.ExitUsage)
}
