package main

import (
	"io"

	"github.com/anivaryam/proc-compose/internal/agentguide"
	"github.com/spf13/cobra"
)

func newAgentGuideCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "agent-guide",
		Short:   "Print the offline operating guide for coding agents",
		Long:    "Print the offline, version-matched instructions for session ownership, readiness, bounded JSON log queries, cursors, failure evidence, and cleanup. This command does not read project configuration or start processes.",
		Example: "  proc-compose agent-guide\n  proc-compose agent-guide > proc-compose-guide.md",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), agentguide.Text)
			return err
		},
	}
}
