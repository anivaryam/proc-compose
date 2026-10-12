package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCommand(configFile *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Inspect the config schema, structured diagnostics, and startup plan",
		Long:  "Inspect configuration without starting anything: print the versioned JSON Schema, structured validation diagnostics with stable codes and locations, or the static startup plan.",
		Args:  cobra.NoArgs,
	}

	schemaCmd := &cobra.Command{
		Use:          "schema",
		Short:        "Print the versioned JSON Schema for proc-compose.yml",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := cmd.OutOrStdout().Write(config.Schema())
			return err
		},
	}

	var checkJSON bool
	checkCmd := &cobra.Command{
		Use:          "check",
		Short:        "Validate the config and report located diagnostics with stable codes",
		Long:         "Validate the config without starting anything. Each diagnostic has a stable code, a YAML path such as processes.api.depends_on[0], line/column when known, a message, and a repair hint. Exits non-zero when any diagnostic is reported.",
		Example:      "  proc-compose config check\n  proc-compose config check --json",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			result := config.Diagnose(resolveConfigExtension(*configFile))
			out := cmd.OutOrStdout()
			if checkJSON {
				if err := writeConfigJSON(out, result); err != nil {
					return err
				}
			} else if result.Valid {
				fmt.Fprintf(out, "ok: %s\n", result.ConfigPath)
			} else {
				for _, d := range result.Diagnostics {
					fmt.Fprintf(out, "%s:%d:%d: %s", result.ConfigPath, d.Line, d.Column, d.Code)
					if d.Path != "" {
						fmt.Fprintf(out, " %s", d.Path)
					}
					fmt.Fprintf(out, ": %s\n", d.Message)
					if d.Hint != "" {
						fmt.Fprintf(out, "  hint: %s\n", d.Hint)
					}
				}
			}
			if !result.Valid {
				cmd.SilenceErrors = checkJSON
				return fmt.Errorf("config check found %d problem(s)", len(result.Diagnostics))
			}
			return nil
		},
	}
	checkCmd.Flags().BoolVar(&checkJSON, "json", false, "emit machine-readable JSON")

	var planJSON bool
	planCmd := &cobra.Command{
		Use:          "plan [processes...]",
		Short:        "Show the resolved startup order without starting anything",
		Long:         "Show the static startup plan for the selected processes and their dependencies: start levels, mode, readiness probe, declared named ports, and ${ports.*}/${urls.*}/${session.*}/${config.*} references. References are resolved at session acquisition, so no port or URL value is shown. Output has \"live\": false; nothing is started.",
		Example:      "  proc-compose config plan\n  proc-compose config plan api --json",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(resolveConfigExtension(*configFile))
			if err != nil {
				return fmt.Errorf("%w (run proc-compose config check for located diagnostics)", err)
			}
			plan, err := config.Plan(cfg, args)
			if err != nil {
				return err
			}
			if planJSON {
				return writeConfigJSON(cmd.OutOrStdout(), plan)
			}
			return printStartupPlan(cmd.OutOrStdout(), plan)
		},
	}
	planCmd.Flags().BoolVar(&planJSON, "json", false, "emit machine-readable JSON")

	command.AddCommand(schemaCmd, checkCmd, planCmd)
	return command
}

func writeConfigJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printStartupPlan(out io.Writer, plan *config.StartupPlan) error {
	fmt.Fprintln(out, "plan (not live): "+plan.Note)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LEVEL\tPROCESS\tMODE\tREADINESS\tDEPENDS ON\tPORTS\tREFERENCES")
	for _, p := range plan.Processes {
		readiness := p.Readiness
		if p.Probe != "" {
			readiness += " " + p.Probe
		}
		ports := make([]string, 0, len(p.Ports))
		for name, preferred := range p.Ports {
			ports = append(ports, fmt.Sprintf("%s=%d", name, preferred))
		}
		sort.Strings(ports)
		refs := make([]string, 0, len(p.References))
		for _, r := range p.References {
			ref := r.Field + "←" + r.Ref
			if !r.Resolvable {
				ref += " (unresolved)"
			}
			refs = append(refs, ref)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Level, p.Name, p.Mode, readiness,
			dashIfEmpty(strings.Join(p.DependsOn, ",")), dashIfEmpty(strings.Join(ports, ",")), dashIfEmpty(strings.Join(refs, ", ")))
	}
	return tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
