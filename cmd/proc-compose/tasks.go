package main

import (
	"fmt"

	"github.com/anivaryam/proc-compose/internal/config"
	"github.com/spf13/cobra"
)

func newTasksCommand(configFile *string) *cobra.Command {
	root := &cobra.Command{Use: "tasks", Short: "Discover and run declared project development tasks"}
	var machine bool
	list := &cobra.Command{
		Use: "list", Short: "List explicit task commands without running them", SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return jobFailure(cmd, machine, fmt.Errorf("tasks list accepts no arguments"))
			}
			path, _, _, _, err := resolveConfigPaths(*configFile)
			if err != nil {
				return jobFailure(cmd, machine, err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				return jobFailure(cmd, machine, err)
			}
			type entry struct {
				Name string `json:"name"`
				config.Task
			}
			result := struct {
				Version    int      `json:"schema_version"`
				Kind       string   `json:"kind"`
				ConfigPath string   `json:"config_path"`
				Resources  []string `json:"resources"`
				Tasks      []entry  `json:"tasks"`
			}{1, "tasks.catalog", path, cfg.Resources, []entry{}}
			for _, name := range config.SortedProcessNames(cfg.Tasks) {
				result.Tasks = append(result.Tasks, entry{Name: name, Task: cfg.Tasks[name]})
			}
			if machine {
				return writeConfigJSON(cmd.OutOrStdout(), result)
			}
			for _, task := range result.Tasks {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", task.Name, task.Description)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&machine, "json", false, "emit the task catalog as JSON")
	usageFlagErrors(list)
	root.AddCommand(list, newExecutionCommand(configFile, "task"))
	return root
}
