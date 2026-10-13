package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RestartPolicies, ProcessModes and HealthActions are the accepted enum
// values; Load and the published schema both read them so the two cannot
// drift.
var (
	RestartPolicies = []string{"never", "on-failure", "always"}
	ProcessModes    = []string{ProcessModeService, ProcessModeTask, ProcessModeTeardown}
	HealthActions   = []string{HealthActionReport, HealthActionRestart}
)

// Stable diagnostic codes. Codes are a machine contract: add new ones, never
// rename or repurpose existing ones.
const (
	CodeReadError          = "read_error"
	CodeYAMLSyntax         = "yaml_syntax"
	CodeUnknownField       = "unknown_field"
	CodeInvalidType        = "invalid_type"
	CodeNoProcesses        = "no_processes"
	CodeInvalidMerge       = "invalid_merge"
	CodeInvalidProcessName = "invalid_process_name"
	CodeMissingCmd         = "missing_cmd"
	CodeInvalidRestart     = "invalid_restart"
	CodeInvalidMode        = "invalid_mode"
	CodeTaskIncompatible   = "task_incompatible_field"
	CodeInvalidValue       = "invalid_value"
	CodeInvalidReadyWhen   = "invalid_ready_when"
	CodeInvalidPort        = "invalid_port"
	CodeEnvFile            = "env_file_unreadable"
	CodeSelfDependency     = "self_dependency"
	CodeUnknownDependency  = "unknown_dependency"
	CodeDependencyCycle    = "dependency_cycle"
	CodeInvalidResource    = "invalid_resource"
	CodeTeardownDependency = "teardown_dependency"
	CodeInvalid            = "invalid_config"
)

var diagnosticHints = map[string]string{
	CodeReadError:          "check that the config path exists and is readable",
	CodeYAMLSyntax:         "fix the YAML syntax near the reported line",
	CodeUnknownField:       "remove the key or correct its spelling; run proc-compose config schema for accepted keys",
	CodeInvalidType:        "change the value to the type the schema declares",
	CodeNoProcesses:        "define at least one entry under processes (or a merge section)",
	CodeInvalidMerge:       "use either merge.client/server/api_prefixes or merge.routes",
	CodeInvalidProcessName: "use [-A-Za-z0-9._] and start/end with an alphanumeric character",
	CodeMissingCmd:         "add a cmd: the shell command that runs this process",
	CodeInvalidRestart:     "use never, on-failure, or always",
	CodeInvalidMode:        "use service, task, or teardown",
	CodeTaskIncompatible:   "remove the field, or change the mode: tasks and teardown processes cannot use it",
	CodeInvalidValue:       "use a non-negative value consistent with the restart policy",
	CodeInvalidReadyWhen:   "set exactly one of ready_when.http, ready_when.tcp, ready_when.cmd, or ready_when.log (a valid Go regex); health_when takes http, tcp, or cmd",
	CodeInvalidPort:        "port names follow process-name rules; preferred ports are 0-65535 (0 = ephemeral)",
	CodeEnvFile:            "create the env_file or fix its path (relative paths resolve from the config directory)",
	CodeSelfDependency:     "remove the process from its own depends_on",
	CodeUnknownDependency:  "name an existing process in depends_on",
	CodeDependencyCycle:    "remove one depends_on edge to break the cycle",
	CodeInvalidResource:    "resource labels must be unique, non-empty, single-line, and at most 256 bytes",
	CodeTeardownDependency: "remove the teardown process from depends_on: it runs only after the stack stops",
}

// ValidationError is the typed error Load returns for rule violations. Its
// message is the human message; Code and Path drive structured diagnostics.
type ValidationError struct {
	Code string
	Path []any // string keys and int sequence indices
	Msg  string
	Err  error
}

func (e *ValidationError) Error() string { return e.Msg }
func (e *ValidationError) Unwrap() error { return e.Err }

func invalid(code string, path []any, cause error, format string, args ...any) error {
	return &ValidationError{Code: code, Path: path, Msg: fmt.Errorf(format, args...).Error(), Err: cause}
}

func sortedProcessNames(processes map[string]Process) []string {
	names := make([]string, 0, len(processes))
	for name := range processes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FormatPath renders a location as "processes.api.depends_on[0]".
func FormatPath(path []any) string {
	var b strings.Builder
	for _, segment := range path {
		switch v := segment.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", v)
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, v)
		}
	}
	return b.String()
}

// Diagnostic is one structured validation finding.
type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Diagnostics is the machine-readable result of Diagnose.
type Diagnostics struct {
	SchemaVersion int          `json:"schema_version"`
	Kind          string       `json:"kind"`
	ConfigPath    string       `json:"config_path"`
	Valid         bool         `json:"valid"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

var yamlLineRe = regexp.MustCompile(`line (\d+): (.*)`)

// Diagnose validates the config at path and returns located diagnostics. It
// reports every unknown key and type error found structurally; rule
// violations come from Load itself (first violation only), so messages are
// identical to what up/validate print.
func Diagnose(path string) (result Diagnostics) {
	result = Diagnostics{SchemaVersion: 1, Kind: "config.diagnostics", ConfigPath: path, Diagnostics: []Diagnostic{}}
	add := func(d Diagnostic) {
		d.Hint = diagnosticHints[d.Code]
		result.Diagnostics = append(result.Diagnostics, d)
	}
	defer func() { result.Valid = len(result.Diagnostics) == 0 }()

	data, err := os.ReadFile(path)
	if err != nil {
		add(Diagnostic{Code: CodeReadError, Message: fmt.Sprintf("cannot read %s: %v", path, err)})
		return result
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		d := Diagnostic{Code: CodeYAMLSyntax, Message: err.Error()}
		if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
			d.Line, _ = strconv.Atoi(m[1])
		}
		add(d)
		return result
	}
	if len(doc.Content) == 0 {
		add(Diagnostic{Code: CodeNoProcesses, Message: fmt.Sprintf("no processes defined in %s", path)})
		return result
	}
	root := doc.Content[0]
	// Decode before walking: the decoder rejects recursive aliases and merges
	// ("anchor ... contains itself") and excessive aliasing, and the walk
	// must not expand those inputs.
	var cfg Config
	var typeErr *yaml.TypeError
	decodeErr := root.Decode(&cfg)
	if decodeErr != nil && !errors.As(decodeErr, &typeErr) {
		add(Diagnostic{Code: CodeInvalidType, Message: decodeErr.Error()})
		return result
	}
	nodes := map[string]*yaml.Node{"": root}
	walkNode(root, reflect.TypeOf(Config{}), nil, nodes, add)
	if typeErr != nil {
		for _, text := range typeErr.Errors {
			d := Diagnostic{Code: CodeInvalidType, Message: text}
			if m := yamlLineRe.FindStringSubmatch(text); m != nil {
				d.Line, _ = strconv.Atoi(m[1])
				d.Message = m[2]
				d.Path = pathAtLine(nodes, d.Line)
			}
			add(d)
		}
	}
	if len(result.Diagnostics) > 0 {
		return result
	}

	if _, err := Load(path); err != nil {
		d := Diagnostic{Code: CodeInvalid, Message: err.Error()}
		var verr *ValidationError
		if errors.As(err, &verr) {
			d.Code, d.Path = verr.Code, FormatPath(verr.Path)
			d.Line, d.Column = locate(nodes, verr.Path)
		}
		add(d)
	}
	return result
}

// walkNode reports keys the decoder would reject (KnownFields) and indexes
// every node by its formatted path for later location lookups. Mappings are
// walked through their effective entries (see effectiveEntries), so merged
// keys belong to the merging mapping's path and overridden values are never
// inspected.
func walkNode(node *yaml.Node, t reflect.Type, path []any, nodes map[string]*yaml.Node, add func(Diagnostic)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	child := func(segment any, n *yaml.Node, ct reflect.Type) {
		p := append(append([]any{}, path...), segment)
		nodes[FormatPath(p)] = n
		walkNode(n, ct, p, nodes, add)
	}
	switch {
	case t.Kind() == reflect.Struct && node.Kind == yaml.MappingNode:
		fields := yamlFields(t)
		entries := effectiveEntries(node)
		for i := 0; i+1 < len(entries); i += 2 {
			key, value := entries[i], entries[i+1]
			field, ok := fields[key.Value]
			if !ok {
				p := FormatPath(append(append([]any{}, path...), key.Value))
				add(Diagnostic{Code: CodeUnknownField, Path: p, Line: key.Line, Column: key.Column,
					Message: fmt.Sprintf("field %s not found in type config.%s", key.Value, t.Name())})
				continue
			}
			child(key.Value, value, field.Type)
		}
	case t.Kind() == reflect.Map && node.Kind == yaml.MappingNode:
		entries := effectiveEntries(node)
		for i := 0; i+1 < len(entries); i += 2 {
			child(entries[i].Value, entries[i+1], t.Elem())
		}
	case t.Kind() == reflect.Slice && node.Kind == yaml.SequenceNode:
		for i, item := range node.Content {
			child(i, item, t.Elem())
		}
	}
}

// effectiveEntries returns a mapping's key/value pairs as yaml.v3 decodes
// them: the mapping's own keys first, then the mappings named by its last
// "<<" value, depth-first in sequence order, where a merged key is skipped
// once any earlier entry or the mapping's own keys (its "<<" included) set
// it. A mapping already being expanded is skipped,
// so recursive merges terminate (the decoder rejects them separately).
func effectiveEntries(node *yaml.Node) []*yaml.Node {
	var entries []*yaml.Node
	var seen map[string]bool
	active := map[*yaml.Node]bool{}
	var visit func(m *yaml.Node)
	visit = func(m *yaml.Node) {
		if active[m] {
			return
		}
		active[m] = true
		defer delete(active, m)
		var merge *yaml.Node
		for i := 0; i+1 < len(m.Content); i += 2 {
			key := m.Content[i]
			if isMergeKey(key) {
				merge = m.Content[i+1]
				continue
			}
			if seen != nil {
				if seen[key.Value] {
					continue
				}
				seen[key.Value] = true
			}
			entries = append(entries, key, m.Content[i+1])
		}
		if merge == nil {
			return
		}
		if seen == nil {
			// Like the decoder's merge, seed from every key of the merging
			// mapping, the "<<" directive included, so an inherited quoted
			// "<<" key is suppressed rather than reported.
			seen = make(map[string]bool, len(m.Content)/2)
			for i := 0; i < len(m.Content); i += 2 {
				seen[m.Content[i].Value] = true
			}
		}
		for _, merged := range mergedMappings(merge) {
			visit(merged)
		}
	}
	visit(node)
	return entries
}

// isMergeKey matches yaml.v3's merge-key test: a "<<" key with the merge tag
// (implicit for a plain "<<", but not for a quoted one).
func isMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Value == "<<" && (key.Tag == "" || key.Tag == "!" || key.ShortTag() == "!!merge")
}

// mergedMappings returns the mappings a merge value pulls in: one mapping or
// alias, or a sequence of them. Anything else is left to the decoder to reject.
func mergedMappings(value *yaml.Node) []*yaml.Node {
	resolve := func(n *yaml.Node) *yaml.Node {
		if n.Kind == yaml.AliasNode && n.Alias != nil {
			return n.Alias
		}
		return n
	}
	value = resolve(value)
	if value.Kind == yaml.MappingNode {
		return []*yaml.Node{value}
	}
	var out []*yaml.Node
	if value.Kind == yaml.SequenceNode {
		for _, item := range value.Content {
			if item = resolve(item); item.Kind == yaml.MappingNode {
				out = append(out, item)
			}
		}
	}
	return out
}

// yamlFields maps yaml keys to struct fields using the same tag rules as yaml.v3.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = f
	}
	return fields
}

// locate returns the position of the deepest existing node along path.
func locate(nodes map[string]*yaml.Node, path []any) (int, int) {
	for n := len(path); n >= 0; n-- {
		if node, ok := nodes[FormatPath(path[:n])]; ok {
			return node.Line, node.Column
		}
	}
	return 0, 0
}

func pathAtLine(nodes map[string]*yaml.Node, line int) string {
	best := ""
	for p, node := range nodes {
		if node.Line == line && len(p) > len(best) {
			best = p
		}
	}
	return best
}

// ─── Schema ───────────────────────────────────────────────────────────────────

// SchemaID is the versioned identifier of the published config schema.
const SchemaID = "https://github.com/anivaryam/proc-compose/schema/v1/proc-compose.schema.json"

// schemaExtras adds constraints the Go types cannot express, keyed by
// "Type.yamlkey" (field) or "Type" (object). Field names come from struct
// tags, so the property set itself has one source of truth.
func schemaExtras() map[string]map[string]any {
	portName := map[string]any{"pattern": processNameRe.String()}
	return map[string]map[string]any{
		"Config":                   {"anyOf": []any{map[string]any{"required": []string{"processes"}}, map[string]any{"required": []string{"merge"}}}},
		"Config.processes":         {"propertyNames": map[string]any{"pattern": processNameRe.String()}},
		"Config.resources":         {"uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}},
		"Process":                  {"required": []string{"cmd"}},
		"Process.cmd":              {"minLength": 1},
		"Process.mode":             {"enum": ProcessModes, "default": ProcessModeService},
		"Process.restart":          {"enum": RestartPolicies, "default": "never"},
		"Process.max_restarts":     {"minimum": 0},
		"Process.shutdown_timeout": {"minimum": 0},
		"Process.ready_timeout":    {"minimum": -1},
		"Process.health_when":      {"not": map[string]any{"required": []string{"log"}}}, // a log line matches once; validateProbe(allowLog=false)
		"Process.health_interval":  {"minimum": 0, "default": DefaultHealthInterval},     // 0 = default, like shutdown_timeout
		"Process.health_action":    {"enum": HealthActions, "default": HealthActionReport},
		"Process.ports":            {"propertyNames": portName, "additionalProperties": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535}},
		"ReadyWhen":                {"minProperties": 1, "maxProperties": 1},
	}
}

// Schema returns the JSON Schema (draft 2020-12) for the YAML accepted by
// Load, generated from the config struct tags.
func Schema() []byte {
	extras := schemaExtras()
	defs := make(map[string]any)
	root := objectSchema(reflect.TypeOf(Config{}), defs, extras)
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaID
	root["title"] = "proc-compose configuration"
	root["$defs"] = defs
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		panic(err) // static data; cannot fail
	}
	return append(out, '\n')
}

func objectSchema(t reflect.Type, defs map[string]any, extras map[string]map[string]any) map[string]any {
	props := make(map[string]any)
	for name, field := range yamlFields(t) {
		prop := typeSchema(field.Type, defs, extras)
		for k, v := range extras[t.Name()+"."+name] {
			prop[k] = v
		}
		props[name] = prop
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	for k, v := range extras[t.Name()] {
		schema[k] = v
	}
	return schema
}

func typeSchema(t reflect.Type, defs map[string]any, extras map[string]map[string]any) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if _, ok := defs[t.Name()]; !ok {
			defs[t.Name()] = nil // reserve before recursing
			defs[t.Name()] = objectSchema(t, defs, extras)
		}
		return map[string]any{"$ref": "#/$defs/" + t.Name()}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem(), defs, extras)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": typeSchema(t.Elem(), defs, extras)}
	}
	panic("config schema: unsupported field type " + t.String())
}

// ─── Startup plan ─────────────────────────────────────────────────────────────

// ReferenceTreatment describes when endpoint references receive values.
const ReferenceTreatment = "resolved at session acquisition"

// EndpointReference is one ${namespace.*} placeholder found in a process field.
type EndpointReference struct {
	Field      string `json:"field"`
	Ref        string `json:"ref"`
	Resolvable bool   `json:"resolvable"`
	Treatment  string `json:"treatment"`
}

// PlannedProcess is one process in the static startup plan.
type PlannedProcess struct {
	Name       string              `json:"name"`
	Level      int                 `json:"level"`
	Mode       string              `json:"mode"`
	Readiness  string              `json:"readiness"`
	Probe      string              `json:"probe,omitempty"`
	DependsOn  []string            `json:"depends_on"`
	Ports      map[string]int      `json:"ports,omitempty"`
	Health     *PlannedHealth      `json:"health,omitempty"`
	References []EndpointReference `json:"references,omitempty"`
}

// PlannedHealth summarises a service's health_when probe.
type PlannedHealth struct {
	Probe    string `json:"probe"`  // http | tcp | cmd
	Target   string `json:"target"` // the probe's URL, address or command
	Interval int    `json:"interval_seconds"`
	Action   string `json:"action"` // report | restart
}

// StartupPlan is the resolved dependency order for a selection. It is derived
// statically from config; Live is always false because nothing was started.
type StartupPlan struct {
	SchemaVersion int              `json:"schema_version"`
	Kind          string           `json:"kind"`
	Live          bool             `json:"live"`
	Note          string           `json:"note"`
	Levels        [][]string       `json:"levels"`
	Processes     []PlannedProcess `json:"processes"`
	// Teardown lists the teardown processes in run order. They run after the
	// stack stops, for the whole stack whatever the filter.
	Teardown []string `json:"teardown,omitempty"`
}

var referenceRe = regexp.MustCompile(`\$\{((?:ports|urls|session|config)\.[^}]*)\}`)

// Plan resolves the selected processes (plus dependencies, via SelectedNames)
// into topological start levels. cfg must come from Load, which guarantees
// known dependencies and no cycles.
func Plan(cfg *Config, filter []string) (*StartupPlan, error) {
	for _, name := range filter {
		if err := CheckSelector(cfg, name); err != nil {
			return nil, err
		}
	}
	names := SelectedNames(cfg, filter)
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	level := make(map[string]int, len(names))
	var depth func(string) int
	depth = func(name string) int {
		if l, ok := level[name]; ok {
			return l
		}
		l := 0
		for _, dep := range cfg.Processes[name].DependsOn {
			l = max(l, depth(dep)+1)
		}
		level[name] = l
		return l
	}
	plan := &StartupPlan{SchemaVersion: 1, Kind: "config.plan", Live: false,
		Note: "static plan from configuration; no process was started and endpoint references are " + ReferenceTreatment}
	for _, name := range names {
		l := depth(name)
		for len(plan.Levels) <= l {
			plan.Levels = append(plan.Levels, []string{})
		}
		plan.Levels[l] = append(plan.Levels[l], name)
	}
	for l, group := range plan.Levels {
		for _, name := range group { // names are already sorted
			proc := cfg.Processes[name]
			p := PlannedProcess{Name: name, Level: l, Mode: proc.EffectiveMode(), Readiness: "none", DependsOn: append([]string{}, proc.DependsOn...), Ports: proc.Ports}
			if p.Mode == ProcessModeTask {
				p.Readiness = "exit"
			} else if rw := proc.ReadyWhen; rw != nil {
				switch {
				case rw.HTTP != "":
					p.Readiness, p.Probe = "http", rw.HTTP
				case rw.TCP != "":
					p.Readiness, p.Probe = "tcp", rw.TCP
				case rw.Cmd != "":
					p.Readiness, p.Probe = "cmd", rw.Cmd
				case rw.Log != "":
					p.Readiness, p.Probe = "log", rw.Log
				}
			}
			if hw := proc.HealthWhen; hw != nil {
				h := &PlannedHealth{Interval: proc.EffectiveHealthInterval(), Action: proc.EffectiveHealthAction()}
				switch {
				case hw.HTTP != "":
					h.Probe, h.Target = "http", hw.HTTP
				case hw.TCP != "":
					h.Probe, h.Target = "tcp", hw.TCP
				case hw.Cmd != "":
					h.Probe, h.Target = "cmd", hw.Cmd
				}
				p.Health = h
			}
			p.References = processReferences(cfg, proc, selected)
			plan.Processes = append(plan.Processes, p)
		}
	}
	plan.Teardown = TeardownNames(cfg)
	return plan, nil
}

// processReferences lists placeholders in the fields session.Plan.Resolve expands.
func processReferences(cfg *Config, proc Process, selected map[string]bool) []EndpointReference {
	fields := [][2]string{{"cmd", proc.Cmd}, {"dir", proc.Dir}}
	keys := make([]string, 0, len(proc.Env))
	for key := range proc.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fields = append(fields, [2]string{"env." + key, proc.Env[key]})
	}
	if rw := proc.ReadyWhen; rw != nil {
		fields = append(fields, [2]string{"ready_when.http", rw.HTTP}, [2]string{"ready_when.tcp", rw.TCP}, [2]string{"ready_when.cmd", rw.Cmd})
	}
	if hw := proc.HealthWhen; hw != nil {
		fields = append(fields, [2]string{"health_when.http", hw.HTTP}, [2]string{"health_when.tcp", hw.TCP}, [2]string{"health_when.cmd", hw.Cmd})
	}
	var refs []EndpointReference
	for _, field := range fields {
		for _, m := range referenceRe.FindAllStringSubmatch(field[1], -1) {
			ok := referenceResolvable(cfg, m[1], selected)
			treatment := ReferenceTreatment
			if !ok {
				treatment = "unresolved: select its dependency or declare its named port"
			}
			refs = append(refs, EndpointReference{Field: field[0], Ref: m[1], Resolvable: ok, Treatment: treatment})
		}
	}
	return refs
}

func referenceResolvable(cfg *Config, ref string, selected map[string]bool) bool {
	switch ref {
	case "session.name", "session.id", "session.dir", "config.dir":
		return true
	}
	namespace, rest, _ := strings.Cut(ref, ".")
	if namespace != "ports" && namespace != "urls" {
		return false
	}
	dot := strings.LastIndex(rest, ".")
	if dot < 0 {
		return false
	}
	process, port := rest[:dot], rest[dot+1:]
	_, declared := cfg.Processes[process].Ports[port]
	return selected[process] && declared
}
