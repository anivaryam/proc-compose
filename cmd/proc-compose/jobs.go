package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anivaryam/proc-compose/internal/jobs"
	"github.com/anivaryam/proc-compose/internal/logs"
	"github.com/spf13/cobra"
)

func jobFailure(cmd *cobra.Command, machine bool, err error) error {
	if machine {
		r := mutationResponse{Operation: cmd.CommandPath()}
		return r.render(cmd, err, 1)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), err)
	return exitCodeError{1}
}

func renderJob(cmd *cobra.Command, view *jobs.View, machine bool, err error, code int) error {
	if view == nil {
		return jobFailure(cmd, machine, err)
	}
	if err != nil {
		view.Message = err.Error()
	}
	if machine {
		if err := writeConfigJSON(cmd.OutOrStdout(), view); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "job %s: %s\n  directory: %s\n", view.ID, view.State, view.Directory)
		if view.Message != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), view.Message)
		}
	}
	if code != 0 {
		return exitCodeError{code}
	}
	return nil
}

func newJobsCommand() *cobra.Command {
	root := &cobra.Command{Use: "jobs", Short: "Retrieve and control persistent development jobs"}
	for _, operation := range []string{"list", "show", "logs", "wait", "cancel"} {
		operation := operation
		var machine bool
		var timeout time.Duration
		var limit int
		var before string
		var logOpts logs.Options
		command := &cobra.Command{
			Use: operation + " JOB_ID", Short: operation + " retained development jobs", SilenceUsage: true, SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				want := 1
				if operation == "list" {
					want = 0
				}
				if len(args) != want {
					return jobFailure(cmd, machine, fmt.Errorf("jobs %s requires %d arguments", operation, want))
				}
				store, err := jobs.Open()
				if err != nil {
					return jobFailure(cmd, machine, err)
				}
				if operation == "list" {
					page, err := store.List(limit, before)
					if err != nil {
						return jobFailure(cmd, machine, err)
					}
					if machine {
						return writeConfigJSON(cmd.OutOrStdout(), page)
					}
					for _, job := range page.Jobs {
						fmt.Fprintf(cmd.OutOrStdout(), "%s  %-14s %s\n", job.ID, job.State, job.ConfigPath)
					}
					return nil
				}
				id := args[0]
				if operation == "logs" {
					logOpts.Tail = logOpts.After == ""
					page, err := store.Logs(id, logOpts)
					if err != nil {
						return jobFailure(cmd, machine, err)
					}
					if machine {
						return writeConfigJSON(cmd.OutOrStdout(), page)
					}
					for _, event := range page.Events {
						fmt.Fprintln(cmd.OutOrStdout(), logs.Text(event))
					}
					return nil
				}
				if operation == "show" {
					view, err := store.Get(id)
					return renderJob(cmd, view, machine, err, 0)
				}
				if timeout < time.Millisecond || timeout > 10*time.Minute {
					return jobFailure(cmd, machine, errors.New("--timeout must be between 1ms and 10m"))
				}
				if operation == "cancel" {
					view, err := store.Cancel(id)
					if err != nil {
						return renderJob(cmd, view, machine, err, 1)
					}
				}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				view, err := store.Wait(ctx, id)
				code := 1
				switch {
				case errors.Is(err, context.DeadlineExceeded):
					code = 3 // this tool call ended, not the job
				case errors.Is(err, context.Canceled):
					code = 130
				case err == nil && view.Terminal && view.Result != nil:
					code = view.Result.ExitCode
				}
				return renderJob(cmd, view, machine, err, code)
			},
		}
		command.Flags().BoolVar(&machine, "json", false, "emit one bounded JSON response")
		usageFlagErrors(command)
		switch operation {
		case "list":
			command.Use = "list"
			command.Flags().IntVarP(&limit, "limit", "n", 20, "maximum jobs (1..100)")
			command.Flags().StringVar(&before, "before", "", "resume before the previous page's next_before ID")
		case "wait", "cancel":
			command.Flags().DurationVar(&timeout, "timeout", time.Minute, "bound this wait (1ms..10m); a wait timeout does not cancel the job")
		case "logs":
			command.Flags().IntVarP(&logOpts.Limit, "tail", "n", 50, "maximum events (1..1000)")
			command.Flags().IntVar(&logOpts.MaxBytes, "max-bytes", 64<<10, "maximum encoded event bytes (1024..1048576)")
			command.Flags().StringVar(&logOpts.After, "after", "", "resume from next_cursor, or start")
		}
		root.AddCommand(command)
	}
	var workerRoot string
	worker := &cobra.Command{
		Use: "_worker JOB_ID", Hidden: true, Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return (&jobs.Store{Root: workerRoot}).Work(ctx, args[0])
		},
	}
	worker.Flags().StringVar(&workerRoot, "root", "", "persistent job root")
	root.AddCommand(worker)
	return root
}
