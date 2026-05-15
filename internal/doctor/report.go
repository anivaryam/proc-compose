package doctor

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

type Service struct {
	Name       string     `json:"name"`
	Command    string     `json:"command"`
	Dir        string     `json:"dir"`
	Port       int        `json:"port"`
	Kind       string     `json:"kind"`
	Confidence Confidence `json:"confidence"`
	Manifest   string     `json:"manifest"`
}

type Finding struct {
	Severity   Severity `json:"severity"`
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Detail     string   `json:"detail,omitempty"`
	Suggestion string   `json:"suggestion,omitempty"`
}

type Report struct {
	ConfigPath    string    `json:"config_path,omitempty"`
	ConfigExists  bool      `json:"config_exists"`
	Services      []Service `json:"services"`
	Findings      []Finding `json:"findings"`
	SuggestedYAML string    `json:"suggested_yaml,omitempty"`
	GeneratedPath string    `json:"generated_path,omitempty"`
}

type Options struct {
	Root       string
	ConfigFile string
	Write      bool
}
