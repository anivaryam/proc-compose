package main

import (
	"fmt"

	"github.com/anivaryam/proc-compose/internal/ipc"
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
		// Args runs before the root pre-run hook. Render its session refusal
		// here so it cannot bypass the JSON renderer in RunE.
		if err == nil {
			err = validateSessionFlag()
		}
		if err != nil {
			return response.render(cmd, err)
		}
		return nil
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if !*machine {
			return err
		}
		return response.render(cmd, err)
	}
}

func (r *mutationResponse) render(cmd *cobra.Command, err error) error {
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
		return exitCodeError{1}
	}
	return nil
}
