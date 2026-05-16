package bootstrap

import "github.com/anivaryam/proc-compose/internal/doctor"

type Mode string

const (
	ModeDryRun      Mode = "dry_run"
	ModeWrite       Mode = "write"
	ModeVerify      Mode = "verify"
	ModeWriteVerify Mode = "write_verify"
)

type Verifier interface {
	Verify(configPath string) (*Verification, error)
}

type Options struct {
	Root       string
	ConfigFile string
	Write      bool
	Overwrite  bool
	Verify     bool
	Verifier   Verifier
}

type Verification struct {
	Started    bool   `json:"started"`
	Ready      bool   `json:"ready"`
	Stopped    bool   `json:"stopped"`
	ElapsedMS  int64  `json:"elapsed_ms,omitempty"`
	LogFile    string `json:"log_file,omitempty"`
	Message    string `json:"message,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
}

type Observation struct {
	Process string `json:"process,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type Suggestion struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type AppliedChange struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type Report struct {
	ConfigPath     string          `json:"config_path"`
	Mode           Mode            `json:"mode"`
	Doctor         *doctor.Report  `json:"doctor,omitempty"`
	Verification   *Verification   `json:"verification,omitempty"`
	Observations   []Observation   `json:"observations"`
	Suggestions    []Suggestion    `json:"suggestions"`
	AppliedChanges []AppliedChange `json:"applied_changes"`
}

func modeFromOptions(opts Options) Mode {
	if opts.Write && opts.Verify {
		return ModeWriteVerify
	}
	if opts.Write {
		return ModeWrite
	}
	if opts.Verify {
		return ModeVerify
	}
	return ModeDryRun
}

func newReport(opts Options, configPath string) *Report {
	return &Report{
		ConfigPath:     configPath,
		Mode:           modeFromOptions(opts),
		Observations:   []Observation{},
		Suggestions:    []Suggestion{},
		AppliedChanges: []AppliedChange{},
	}
}
