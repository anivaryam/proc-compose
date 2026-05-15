package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/anivaryam/proc-compose/internal/config"
	"gopkg.in/yaml.v3"
)

// renderSuggestedYAML generates a proc-compose YAML config from detected services.
// It skips low-confidence services and adds merge section when frontend+backend detected.
func renderSuggestedYAML(services []Service) string {
	if len(services) == 0 {
		return "processes: {}\n"
	}

	// Filter low-confidence services
	filtered := make([]Service, 0)
	for _, s := range services {
		if s.Confidence == ConfidenceLow {
			continue
		}
		filtered = append(filtered, s)
	}

	if len(filtered) == 0 {
		return "processes: {}\n"
	}

	processNames := uniqueProcessNames(filtered)

	// Detect distinct frontend and backend services for merge-port.
	frontendIndexes := []int{}
	backendIndexes := []int{}
	for i, s := range filtered {
		if isFrontend(s) && s.Port > 0 {
			frontendIndexes = append(frontendIndexes, i)
		}
		if isBackend(s) && s.Port > 0 {
			backendIndexes = append(backendIndexes, i)
		}
	}

	// Build config
	cfg := &config.Config{
		Processes: make(map[string]config.Process),
	}

	if len(frontendIndexes) == 1 && len(backendIndexes) == 1 && filtered[frontendIndexes[0]].Port != filtered[backendIndexes[0]].Port {
		cfg.Merge = &config.Merge{
			Client: filtered[frontendIndexes[0]].Port,
			Server: filtered[backendIndexes[0]].Port,
		}
	}

	for i, s := range filtered {
		proc := config.Process{
			Cmd: s.Command,
		}
		if s.Dir != "." {
			proc.Dir = s.Dir
		}
		if s.Port > 0 {
			if proc.Env == nil {
				proc.Env = make(map[string]string)
			}
			proc.Env["PORT"] = fmt.Sprintf("%d", s.Port)
			proc.ReadyWhen = &config.ReadyWhen{
				TCP: fmt.Sprintf("localhost:%d", s.Port),
			}
		}
		cfg.Processes[processNames[i]] = proc
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return "processes: {}\n"
	}
	return buf.String()
}

func uniqueProcessNames(services []Service) []string {
	names := make([]string, len(services))
	used := map[string]int{}
	for i, service := range services {
		name := sanitizeProcessName(service.Name)
		if used[name] == 0 {
			names[i] = name
			used[name] = 1
			continue
		}
		base := name
		if service.Dir != "" && service.Dir != "." {
			base = sanitizeProcessName(strings.TrimPrefix(service.Dir, "./"))
		}
		if base == name {
			base = fmt.Sprintf("%s-%d", name, used[name]+1)
		}
		candidate := base
		for used[candidate] > 0 {
			used[name]++
			candidate = fmt.Sprintf("%s-%d", base, used[name])
		}
		names[i] = candidate
		used[candidate] = 1
		used[name]++
	}
	return names
}

// isFrontend returns true if the service looks like a frontend.
func isFrontend(s Service) bool {
	name := strings.ToLower(s.Name)
	if strings.Contains(name, "front") || strings.Contains(name, "web") || strings.Contains(name, "client") || strings.Contains(name, "ui") || strings.Contains(name, "app") {
		return true
	}
	// Known frontend dev-server port.
	if s.Port == 5173 {
		return true
	}
	return false
}

// isBackend returns true if the service looks like a backend.
func isBackend(s Service) bool {
	name := strings.ToLower(s.Name)
	if strings.Contains(name, "api") || strings.Contains(name, "back") || strings.Contains(name, "server") {
		return true
	}
	// Go services are typically backends
	if s.Kind == "go" {
		return true
	}
	return false
}

// WriteText writes a human-readable diagnosis report to the given writer.
func WriteText(w io.Writer, report *Report) error {
	if len(report.Services) > 0 {
		fmt.Fprintf(w, "Detected services:\n")
		for _, s := range report.Services {
			fmt.Fprintf(w, "  • %s  (%s)  cmd: %s\n", s.Name, s.Kind, s.Command)
		}
		fmt.Fprintln(w)
	}
	if len(report.Findings) > 0 {
		fmt.Fprintf(w, "Findings:\n")
		for _, f := range report.Findings {
			fmt.Fprintf(w, "  [%s] %s: %s\n", f.Severity, f.Code, f.Message)
			if f.Suggestion != "" {
				fmt.Fprintf(w, "    → %s\n", f.Suggestion)
			}
		}
		fmt.Fprintln(w)
	}
	if report.ConfigExists {
		fmt.Fprintf(w, "Config: %s (exists)\n", report.ConfigPath)
	} else {
		fmt.Fprintf(w, "Config: %s (not found)\n", report.ConfigPath)
	}
	if report.GeneratedPath != "" {
		fmt.Fprintf(w, "Generated: %s\n", report.GeneratedPath)
	}
	if report.SuggestedYAML != "" && report.SuggestedYAML != "processes: {}\n" {
		fmt.Fprintln(w, "Suggested config:")
		_, err := io.WriteString(w, report.SuggestedYAML)
		return err
	}
	return nil
}

// WriteJSON writes the report as JSON to the given writer.
func WriteJSON(w io.Writer, report *Report) error {
	if report == nil {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
