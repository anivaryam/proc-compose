package bootstrap

import (
	"encoding/json"
	"fmt"
	"io"
)

func WriteText(w io.Writer, report *Report) error {
	if report == nil {
		return nil
	}
	fmt.Fprintf(w, "proc-compose bootstrap\n\n")
	fmt.Fprintf(w, "mode: %s\n", report.Mode)
	fmt.Fprintf(w, "config: %s\n", report.ConfigPath)
	if report.Doctor != nil && report.Doctor.SuggestedYAML != "" && report.Mode == ModeDryRun {
		fmt.Fprintln(w, "\nSuggested config:")
		if _, err := io.WriteString(w, report.Doctor.SuggestedYAML); err != nil {
			return err
		}
	}
	if report.Verification != nil {
		fmt.Fprintf(w, "\nverification: started=%t ready=%t stopped=%t\n", report.Verification.Started, report.Verification.Ready, report.Verification.Stopped)
		if report.Verification.Message != "" {
			fmt.Fprintf(w, "  %s\n", report.Verification.Message)
		}
	}
	if len(report.AppliedChanges) > 0 {
		fmt.Fprintln(w, "\nApplied changes:")
		for _, change := range report.AppliedChanges {
			fmt.Fprintf(w, "  - %s: %s\n", change.Path, change.Message)
		}
	}
	if len(report.Observations) > 0 {
		fmt.Fprintln(w, "\nObservations:")
		for _, observation := range report.Observations {
			fmt.Fprintf(w, "  - [%s] %s\n", observation.Kind, observation.Message)
		}
	}
	if len(report.Suggestions) > 0 {
		fmt.Fprintln(w, "\nSuggestions:")
		for _, suggestion := range report.Suggestions {
			fmt.Fprintf(w, "  - [%s] %s\n", suggestion.Code, suggestion.Message)
		}
	}
	return nil
}

func WriteJSON(w io.Writer, report *Report) error {
	if report == nil {
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
