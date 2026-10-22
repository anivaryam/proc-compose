package config

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proc-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiagnose_Codes(t *testing.T) {
	tests := []struct {
		name, yaml, code, path string
		line                   int
	}{
		{"yaml syntax", "processes:\n\tapi: 1\n", CodeYAMLSyntax, "", 2},
		{"unknown root field", "processes:\n  api:\n    cmd: x\ntunnels: 1\n", CodeUnknownField, "tunnels", 4},
		{"unknown process field", "processes:\n  api:\n    cmd: x\n    comand: y\n", CodeUnknownField, "processes.api.comand", 4},
		{"unknown ready_when field", "processes:\n  api:\n    cmd: x\n    ready_when:\n      grpc: x\n", CodeUnknownField, "processes.api.ready_when.grpc", 5},
		{"invalid type", "processes:\n  api:\n    cmd: x\n    max_restarts: lots\n", CodeInvalidType, "processes.api.max_restarts", 4},
		{"empty document", "", CodeNoProcesses, "", 0},
		{"missing cmd", "processes:\n  api:\n    dir: .\n", CodeMissingCmd, "processes.api", 3},
		{"unknown dependency", "processes:\n  api:\n    cmd: x\n    depends_on: [db, cache]\n  db:\n    cmd: y\n", CodeUnknownDependency, "processes.api.depends_on[1]", 4},
		{"self dependency", "processes:\n  api:\n    cmd: x\n    depends_on:\n      - api\n", CodeSelfDependency, "processes.api.depends_on[0]", 5},
		{"dependency cycle", "processes:\n  a:\n    cmd: x\n    depends_on: [b]\n  b:\n    cmd: y\n    depends_on: [a]\n", CodeDependencyCycle, "processes.b.depends_on[0]", 7},
		{"task ready_when", "processes:\n  migrate:\n    cmd: x\n    mode: task\n    ready_when:\n      log: done\n", CodeTaskIncompatible, "processes.migrate.ready_when", 6},
		{"task restart", "processes:\n  migrate:\n    cmd: x\n    mode: task\n    restart: always\n", CodeTaskIncompatible, "processes.migrate.restart", 5},
		{"invalid restart", "processes:\n  api:\n    cmd: x\n    restart: sometimes\n", CodeInvalidRestart, "processes.api.restart", 4},
		{"invalid mode", "processes:\n  api:\n    cmd: x\n    mode: daemon\n", CodeInvalidMode, "processes.api.mode", 4},
		{"teardown ports", "processes:\n  clean:\n    cmd: x\n    mode: teardown\n    ports:\n      http: 0\n", CodeTaskIncompatible, "processes.clean.ports", 6},
		{"teardown dependency", "processes:\n  api:\n    cmd: x\n    depends_on: [clean]\n  clean:\n    cmd: y\n    mode: teardown\n", CodeTeardownDependency, "processes.api.depends_on[0]", 4},
		{"invalid port", "processes:\n  api:\n    cmd: x\n    ports:\n      http: 70000\n", CodeInvalidPort, "processes.api.ports.http", 5},
		{"invalid ready_when", "processes:\n  api:\n    cmd: x\n    ready_when:\n      http: http://x\n      tcp: x:1\n", CodeInvalidReadyWhen, "processes.api.ready_when", 5},
		{"invalid resource", "processes:\n  api:\n    cmd: x\nresources: [db, db]\n", CodeInvalidResource, "resources[1]", 4},
		{"invalid merge", "merge:\n  routes: [\"/api=1\"]\n  client: 3000\n", CodeInvalidMerge, "merge", 2},
		{"merge port out of range", "merge:\n  client: 3000\n  server: 3001\n  port: 70000\nprocesses:\n  web:\n    cmd: x\n", CodeInvalidMerge, "merge.port", 4},
		{"merge client out of range", "merge:\n  client: 99999\n  server: 3001\nprocesses:\n  web:\n    cmd: x\n", CodeInvalidMerge, "merge.client", 2},
		{"merge server out of range", "merge:\n  client: 3000\n  server: 70000\nprocesses:\n  web:\n    cmd: x\n", CodeInvalidMerge, "merge.server", 3},
		{"merge route target out of range", "merge:\n  routes:\n    - /=3000\n    - /api=99999\nprocesses:\n  web:\n    cmd: x\n", CodeInvalidMerge, "merge.routes[1]", 4},
		{"merge client_process names a task", "merge:\n  client: 3000\n  server: 3001\n  client_process: build\nprocesses:\n  build:\n    cmd: x\n    mode: task\n", CodeInvalidMerge, "merge.client_process", 4},
		{"unknown field pulled in by merge key", "processes:\n  copy:\n    <<: &d\n      cmd: x\n      bogus: 1\n", CodeUnknownField, "processes.copy.bogus", 5},
		{"invalid merged field located at its anchor", "processes:\n  base: &d\n    cmd: x\n    restart: always\n  copy:\n    <<: *d\n    mode: task\n", CodeTaskIncompatible, "processes.copy.restart", 4},
		{"earlier merge source wins and is located", "processes:\n  a: &a\n    cmd: x\n    restart: always\n  b: &b\n    cmd: y\n    restart: never\n  job:\n    <<: [*a, *b]\n    mode: task\n", CodeTaskIncompatible, "processes.job.restart", 4},
		{"effective nested merged unknown field", "processes:\n  api:\n    <<:\n      - ready_when:\n          grpc: x\n      - ready_when:\n          log: ready\n    cmd: x\n", CodeUnknownField, "processes.api.ready_when.grpc", 5},
		{"quoted merge key is an ordinary key", "processes:\n  api:\n    cmd: x\n    \"<<\": {restart: never}\n", CodeUnknownField, "processes.api.<<", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Diagnose(writeConfig(t, tt.yaml))
			if result.Valid || len(result.Diagnostics) == 0 {
				t.Fatalf("expected diagnostics, got %+v", result)
			}
			d := result.Diagnostics[0]
			if d.Code != tt.code || d.Path != tt.path || d.Line != tt.line {
				t.Fatalf("got code=%s path=%q line=%d (%s); want code=%s path=%q line=%d", d.Code, d.Path, d.Line, d.Message, tt.code, tt.path, tt.line)
			}
			if d.Message == "" || d.Hint == "" {
				t.Fatalf("diagnostic lacks message or hint: %+v", d)
			}
		})
	}
}

// TestDiagnosticHints_NameTheRuleTheyExplain pins the hints that state a rule:
// the invalid_merge hint names the refused upstream PORT, the port range and
// the client_process rule; the invalid_port hint says that a dot is not
// allowed in a port name; the invalid_value hint gives ready_timeout its -1
// floor instead of calling every value non-negative; and the
// env_file_unreadable hint names the line limit.
func TestDiagnosticHints_NameTheRuleTheyExplain(t *testing.T) {
	for code, rules := range map[string][]string{
		CodeInvalidMerge: {"PORT", "merge port", "1-65535", "merge.client_process must name a service"},
		CodeInvalidPort:  {"`.` is not allowed in port names"},
		CodeInvalidValue: {"ready_timeout >= -1 (-1 = no limit)", "shutdown_timeout <= 5"},
		CodeEnvFile:      {"at most 1 MiB"},
	} {
		for _, rule := range rules {
			if !strings.Contains(diagnosticHints[code], rule) {
				t.Errorf("%s hint omits %q: %q", code, rule, diagnosticHints[code])
			}
		}
	}
	if strings.Contains(diagnosticHints[CodeInvalidValue], "non-negative") {
		t.Errorf("invalid_value hint calls every value non-negative, but ready_timeout: -1 is valid: %q", diagnosticHints[CodeInvalidValue])
	}
}

// TestDiagnose_MergeKeysAndAliasesMatchLoad keeps structured diagnostics in
// agreement with the loader for YAML merge keys and aliases.
func TestDiagnose_MergeKeysAndAliasesMatchLoad(t *testing.T) {
	valid := map[string]string{
		"merge alias":          "processes:\n  base: &defaults\n    cmd: \"sleep 60\"\n  copy:\n    <<: *defaults\n",
		"merge sequence":       "processes:\n  a: &a\n    cmd: x\n  b: &b\n    cmd: y\n    restart: never\n  c:\n    <<: [*a, *b]\n",
		"explicit key wins":    "processes:\n  base: &d\n    cmd: x\n    restart: always\n  job:\n    <<: *d\n    mode: task\n    restart: never\n",
		"map merge":            "processes:\n  api:\n    cmd: x\n    env:\n      <<: &env\n        A: \"1\"\n      B: \"2\"\n",
		"alias as whole value": "processes:\n  base: &d\n    cmd: x\n  copy: *d\n",
		"explicit key replaces merged nested value": "processes:\n  api:\n    <<:\n      ready_when:\n        unsupported: ignored\n    cmd: \"sleep 60\"\n    ready_when:\n      log: ready\n",
		"earlier sequence entry replaces later":     "processes:\n  api:\n    <<:\n      - ready_when:\n          log: ready\n      - ready_when:\n          unsupported: ignored\n    cmd: x\n",
		"nested merge chain":                        "processes:\n  base: &base\n    cmd: x\n    restart: never\n  mid: &mid\n    <<: *base\n    mode: task\n  job:\n    <<: *mid\n",
		"alias reused many times":                   "processes:\n  base: &d\n    cmd: x\n  a: *d\n  b: *d\n  c:\n    <<: [*d, *d]\n    env: {A: \"1\"}\n",
		"inherited quoted merge key suppressed":     "processes:\n  api:\n    <<:\n      cmd: \"sleep 60\"\n      \"<<\": ignored\n",
		"quoted merge key in map is ordinary":       "processes:\n  api:\n    cmd: x\n    env:\n      \"<<\": \"1\"\n",
	}
	for name, content := range valid {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err != nil {
				t.Fatalf("fixture rejected by Load: %v", err)
			}
			if result := Diagnose(path); !result.Valid {
				t.Fatalf("Load accepts but Diagnose rejects: %+v", result.Diagnostics)
			}
		})
	}
	invalid := []string{
		"processes:\n  copy:\n    <<: &d\n      cmd: x\n      bogus: 1\n",
		"processes:\n  base: &d\n    cmd: x\n    restart: always\n  copy:\n    <<: *d\n    mode: task\n",
		"processes:\n  a: &a\n    cmd: x\n    restart: always\n  b: &b\n    cmd: y\n    restart: never\n  job:\n    <<: [*a, *b]\n    mode: task\n",
		"processes:\n  api:\n    <<:\n      - ready_when:\n          grpc: x\n      - ready_when:\n          log: ready\n    cmd: x\n",
		"processes:\n  api:\n    cmd: x\n    \"<<\": {restart: never}\n",
	}
	for _, content := range invalid {
		path := writeConfig(t, content)
		if _, err := Load(path); err == nil {
			t.Fatalf("fixture accepted by Load:\n%s", content)
		}
		if result := Diagnose(path); result.Valid {
			t.Fatalf("Load rejects but Diagnose accepts:\n%s", content)
		}
	}
}

// TestDiagnoseHelper runs Diagnose in a child process so a recursive input
// that hangs or overflows the stack fails one test instead of the runner.
func TestDiagnoseHelper(t *testing.T) {
	path := os.Getenv("PROC_COMPOSE_DIAGNOSE_HELPER")
	if path == "" {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(Diagnose(path))
	os.Exit(0)
}

func TestDiagnose_RecursiveMergesRejectedPromptly(t *testing.T) {
	recursive := map[string]string{
		"self merge":              "processes:\n  api: &self\n    cmd: \"sleep 60\"\n    <<: *self\n",
		"self merge in sequence":  "processes:\n  api: &self\n    cmd: x\n    <<: [{restart: never}, *self]\n",
		"indirect through inline": "processes:\n  api: &a\n    cmd: x\n    <<:\n      <<: *a\n",
		"indirect through anchor": "processes:\n  api: &a\n    cmd: x\n    <<: &b\n      <<: *a\n  web:\n    <<: *b\n",
	}
	for name, content := range recursive {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Fatal("fixture accepted by Load")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiagnoseHelper$")
			child.Env = append(os.Environ(), "PROC_COMPOSE_DIAGNOSE_HELPER="+path)
			start := time.Now()
			out, err := child.Output()
			if err != nil {
				t.Fatalf("Diagnose did not finish cleanly after %v: %v (ctx: %v)", time.Since(start), err, ctx.Err())
			}
			var result Diagnostics
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("helper output: %v\n%s", err, out)
			}
			if result.Valid || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeInvalidType ||
				!strings.Contains(result.Diagnostics[0].Message, "contains itself") {
				t.Fatalf("want one structured recursion diagnostic, got %+v", result)
			}
		})
	}
}

func TestDiagnose_MessagesMatchLoad(t *testing.T) {
	path := writeConfig(t, "processes:\n  a:\n    cmd: x\n    depends_on: [b]\n  b:\n    cmd: y\n    depends_on: [a]\n")
	_, err := Load(path)
	result := Diagnose(path)
	if err == nil || result.Diagnostics[0].Message != err.Error() {
		t.Fatalf("diagnostic %q diverged from Load error %v", result.Diagnostics[0].Message, err)
	}
}

func TestDiagnose_ReportsAllUnknownFieldsWithColumns(t *testing.T) {
	result := Diagnose(writeConfig(t, "processes:\n  api:\n    cmd: x\n    bogus: 1\n  web:\n    cmd: y\n    other: 2\n"))
	if len(result.Diagnostics) != 2 {
		t.Fatalf("want 2 diagnostics, got %+v", result.Diagnostics)
	}
	for _, d := range result.Diagnostics {
		if d.Code != CodeUnknownField || d.Column != 5 {
			t.Fatalf("unexpected diagnostic %+v", d)
		}
	}
}

func TestDiagnose_ValidConfigAndReadError(t *testing.T) {
	result := Diagnose(writeConfig(t, "processes:\n  api:\n    cmd: x\n"))
	if !result.Valid || len(result.Diagnostics) != 0 || result.SchemaVersion != 1 || result.Kind != "config.diagnostics" {
		t.Fatalf("unexpected result %+v", result)
	}
	result = Diagnose(filepath.Join(t.TempDir(), "missing.yml"))
	if result.Valid || result.Diagnostics[0].Code != CodeReadError {
		t.Fatalf("unexpected result %+v", result)
	}
}

// tagNames walks yaml tags independently of the schema generator.
func tagNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			names = append(names, tag)
		}
	}
	sort.Strings(names)
	return names
}

func TestSchema_ParityWithStructTagsAndParser(t *testing.T) {
	var schema struct {
		ID         string                    `json:"$id"`
		Properties map[string]any            `json:"properties"`
		Defs       map[string]map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema.ID, "/v1/") {
		t.Fatalf("schema $id is not versioned: %s", schema.ID)
	}
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	objects := map[string]map[string]any{"Config": schema.Properties}
	for _, typ := range []reflect.Type{reflect.TypeOf(Process{}), reflect.TypeOf(ReadyWhen{}), reflect.TypeOf(Merge{})} {
		props, _ := schema.Defs[typ.Name()]["properties"].(map[string]any)
		objects[typ.Name()] = props
	}
	types := map[string]reflect.Type{"Config": reflect.TypeOf(Config{}), "Process": reflect.TypeOf(Process{}), "ReadyWhen": reflect.TypeOf(ReadyWhen{}), "Merge": reflect.TypeOf(Merge{})}
	for name, typ := range types {
		if got, want := keys(objects[name]), tagNames(typ); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s schema properties %v != yaml tags %v", name, got, want)
		}
	}
	if len(schema.Defs) != 3 {
		t.Fatalf("unexpected $defs: %v", keys(map[string]any{"defs": schema.Defs}))
	}

	// Parser parity: every schema property of Process is accepted by the decoder.
	var yamlText strings.Builder
	yamlText.WriteString("processes:\n  api:\n")
	for _, key := range keys(objects["Process"]) {
		yamlText.WriteString("    " + key + ": null\n")
	}
	for _, d := range Diagnose(writeConfig(t, yamlText.String())).Diagnostics {
		if d.Code == CodeUnknownField {
			t.Fatalf("schema property rejected by parser: %+v", d)
		}
	}
	// Enum parity: every schema restart value loads; others are rejected.
	for _, policy := range RestartPolicies {
		if _, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\n    restart: "+policy+"\n")); err != nil {
			t.Fatalf("schema restart %q rejected: %v", policy, err)
		}
	}
	// health_action enum parity, and the health_interval floor.
	process := schema.Defs["Process"]["properties"].(map[string]any)
	action, _ := process["health_action"].(map[string]any)
	if enum, _ := action["enum"].([]any); len(enum) != len(HealthActions) {
		t.Fatalf("health_action schema = %v, want enum %v", action, HealthActions)
	}
	for _, a := range HealthActions {
		if _, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\n    restart: always\n    health_when:\n      tcp: localhost:1\n    health_action: "+a+"\n")); err != nil {
			t.Fatalf("schema health_action %q rejected: %v", a, err)
		}
	}
	// health_interval floor parity: Load accepts the schema minimum and
	// rejects the value below it.
	interval, _ := process["health_interval"].(map[string]any)
	floor, ok := interval["minimum"].(float64)
	if !ok {
		t.Fatalf("health_interval schema = %v, want a minimum", interval)
	}
	healthYAML := func(extra string) string {
		return "processes:\n  api:\n    cmd: x\n    health_when:\n      tcp: localhost:1\n" + extra
	}
	if _, err := Load(writeConfig(t, healthYAML(fmt.Sprintf("    health_interval: %d\n", int(floor))))); err != nil {
		t.Fatalf("schema minimum health_interval %v rejected by Load: %v", floor, err)
	}
	if _, err := Load(writeConfig(t, healthYAML(fmt.Sprintf("    health_interval: %d\n", int(floor)-1)))); err == nil {
		t.Fatalf("health_interval %d is below the schema minimum %v but Load accepted it", int(floor)-1, floor)
	}
	// ready_timeout floor parity: Load accepts the schema minimum (-1 = no
	// limit) and rejects the value below it.
	readyTimeout, _ := process["ready_timeout"].(map[string]any)
	readyFloor, ok := readyTimeout["minimum"].(float64)
	if !ok {
		t.Fatalf("ready_timeout schema = %v, want a minimum", readyTimeout)
	}
	readyYAML := func(extra string) string {
		return "processes:\n  api:\n    cmd: x\n" + extra
	}
	if _, err := Load(writeConfig(t, readyYAML(fmt.Sprintf("    ready_timeout: %d\n", int(readyFloor))))); err != nil {
		t.Fatalf("schema minimum ready_timeout %v rejected by Load: %v", readyFloor, err)
	}
	if _, err := Load(writeConfig(t, readyYAML(fmt.Sprintf("    ready_timeout: %d\n", int(readyFloor)-1)))); err == nil {
		t.Fatalf("ready_timeout %d is below the schema minimum %v but Load accepted it", int(readyFloor)-1, readyFloor)
	}
	// Probe kind parity: for ready_when and health_when, the schema forbids a
	// kind (not.required) exactly when Load rejects it.
	probeValues := map[string]string{"http": "http://localhost:1/", "tcp": "localhost:1", "cmd": "true", "log": "ready"}
	if got, want := keys(objects["ReadyWhen"]), keys(map[string]any{"http": 0, "tcp": 0, "cmd": 0, "log": 0}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadyWhen kinds %v, probe parity covers %v", got, want)
	}
	for _, field := range []string{"ready_when", "health_when"} {
		prop, _ := process[field].(map[string]any)
		forbidden := map[string]bool{}
		if not, ok := prop["not"].(map[string]any); ok {
			req, _ := not["required"].([]any)
			for _, k := range req {
				forbidden[k.(string)] = true
			}
		}
		for kind, value := range probeValues {
			_, err := Load(writeConfig(t, "processes:\n  api:\n    cmd: x\n    "+field+":\n      "+kind+": "+value+"\n"))
			if schemaOK, loadOK := !forbidden[kind], err == nil; schemaOK != loadOK {
				t.Fatalf("%s.%s: schema accepts=%v, Load accepts=%v (err %v)", field, kind, schemaOK, loadOK, err)
			}
		}
	}
}

// TestPlan_ListsTeardownSeparately pins that teardown processes never enter
// the startup levels; the plan lists them on their own, in run order, and
// refuses one named as a selector.
func TestPlan_ListsTeardownSeparately(t *testing.T) {
	cfg := loadFromString(t, `processes:
  api:
    cmd: api
  zz-cleanup:
    mode: teardown
    cmd: cleanup
  a-cleanup:
    mode: teardown
    cmd: cleanup
`)
	plan, err := Plan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Levels) != 1 || !equalSlices(plan.Levels[0], []string{"api"}) || len(plan.Processes) != 1 {
		t.Fatalf("plan = %+v, want only api in the startup levels", plan)
	}
	if !equalSlices(plan.Teardown, []string{"a-cleanup", "zz-cleanup"}) {
		t.Fatalf("plan.Teardown = %v, want both teardown processes in name order", plan.Teardown)
	}
	if plan, err := Plan(cfg, []string{"api"}); err != nil || !equalSlices(plan.Teardown, []string{"a-cleanup", "zz-cleanup"}) {
		t.Fatalf("filtered plan = %+v, %v; teardown runs for the stack under a filter too", plan, err)
	}
	if _, err := Plan(cfg, []string{"a-cleanup"}); err == nil || !strings.Contains(err.Error(), "teardown") {
		t.Fatalf("Plan(a-cleanup) = %v, want a teardown refusal", err)
	}
}

func TestPlan_LevelsReadinessAndReferences(t *testing.T) {
	cfg := loadFromString(t, `processes:
  db:
    cmd: postgres -p ${ports.db.pg}
    ports:
      pg: 0
    ready_when:
      tcp: localhost:${ports.db.pg}
  migrate:
    cmd: migrate
    mode: task
    depends_on: [db]
    env:
      DATABASE_URL: postgres://localhost:${ports.db.pg}/app
  api:
    cmd: api --dir ${session.dir}
    depends_on: [migrate, db]
    env:
      CACHE: ${urls.cache.http}
    ready_when:
      http: ${urls.api.http}/health
    ports:
      http: 3000
  cache:
    cmd: redis
`)
	plan, err := Plan(cfg, []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Live || plan.Kind != "config.plan" || plan.SchemaVersion != 1 {
		t.Fatalf("plan must be marked non-live: %+v", plan)
	}
	if want := [][]string{{"db"}, {"migrate"}, {"api"}}; !reflect.DeepEqual(plan.Levels, want) {
		t.Fatalf("levels = %v, want %v", plan.Levels, want)
	}
	byName := map[string]PlannedProcess{}
	for _, p := range plan.Processes {
		byName[p.Name] = p
	}
	if byName["db"].Readiness != "tcp" || byName["migrate"].Readiness != "exit" || byName["api"].Readiness != "http" {
		t.Fatalf("unexpected readiness: %+v", plan.Processes)
	}
	refs := byName["api"].References
	want := []EndpointReference{
		{Field: "cmd", Ref: "session.dir", Resolvable: true, Treatment: ReferenceTreatment},
		{Field: "env.CACHE", Ref: "urls.cache.http", Resolvable: false, Treatment: refs[1].Treatment},
		{Field: "ready_when.http", Ref: "urls.api.http", Resolvable: true, Treatment: ReferenceTreatment},
	}
	if !reflect.DeepEqual(refs, want) || !strings.HasPrefix(refs[1].Treatment, "unresolved") {
		t.Fatalf("references = %+v", refs)
	}

	worker := loadFromString(t, "processes:\n  worker:\n    cmd: worker\n    ports:\n      http: 0\n    ready_when:\n      cmd: check ${urls.worker.http}/ready\n")
	cmdPlan, err := Plan(worker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := cmdPlan.Processes[0]; p.Readiness != "cmd" || p.Probe != "check ${urls.worker.http}/ready" ||
		!reflect.DeepEqual(p.References, []EndpointReference{{Field: "ready_when.cmd", Ref: "urls.worker.http", Resolvable: true, Treatment: ReferenceTreatment}}) {
		t.Fatalf("cmd probe plan = %+v", p)
	}
	if !strings.Contains(diagnosticHints[CodeInvalidReadyWhen], "ready_when.cmd") {
		t.Fatalf("ready_when hint omits cmd: %q", diagnosticHints[CodeInvalidReadyWhen])
	}
	if !strings.Contains(diagnosticHints[CodeInvalidReadyWhen], "health_when") {
		t.Fatalf("ready_when hint omits health_when: %q", diagnosticHints[CodeInvalidReadyWhen])
	}

	healthCfg := loadFromString(t, "processes:\n  worker:\n    cmd: worker\n    restart: on-failure\n    ports:\n      http: 0\n    health_when:\n      cmd: check ${urls.worker.http}/live\n    health_interval: 15\n    health_action: restart\n  db:\n    cmd: db\n")
	healthPlan, err := Plan(healthCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range healthPlan.Processes {
		switch p.Name {
		case "worker":
			if want := (&PlannedHealth{Probe: "cmd", Target: "check ${urls.worker.http}/live", Interval: 15, Action: HealthActionRestart}); !reflect.DeepEqual(p.Health, want) ||
				!reflect.DeepEqual(p.References, []EndpointReference{{Field: "health_when.cmd", Ref: "urls.worker.http", Resolvable: true, Treatment: ReferenceTreatment}}) {
				t.Fatalf("health plan = %+v / %+v", p.Health, p.References)
			}
		case "db":
			if p.Health != nil {
				t.Fatalf("unmonitored process planned with health: %+v", p.Health)
			}
		}
	}

	all, err := Plan(cfg, nil)
	if err != nil || len(all.Processes) != 4 || !reflect.DeepEqual(all.Levels[0], []string{"cache", "db"}) {
		t.Fatalf("full plan = %+v, %v", all, err)
	}
	if _, err := Plan(cfg, []string{"nope"}); err == nil {
		t.Fatal("unknown filter accepted")
	}
}

// TestPlan_DottedProcessNameResolvesItsPort pins that a dotted process name
// with a dot-free port name resolves to that process's port: the plan and the
// session split the reference name at the last dot, so they agree.
func TestPlan_DottedProcessNameResolvesItsPort(t *testing.T) {
	cfg := loadFromString(t, "processes:\n  a.b:\n    cmd: serve ${ports.a.b.c}\n    ports:\n      c: 0\n")
	plan, err := Plan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []EndpointReference{{Field: "cmd", Ref: "ports.a.b.c", Resolvable: true, Treatment: ReferenceTreatment}}
	if !reflect.DeepEqual(plan.Processes[0].References, want) {
		t.Fatalf("references = %+v, want %+v", plan.Processes[0].References, want)
	}
}

// ─── #56: the published schema agrees with Load ───────────────────────────────

// schemaDivergences are the only places where the published schema and Load
// may disagree, keyed by the rule name a parity sample carries. Each is listed
// in docs/configuration.md with the wording in doc.
var schemaDivergences = map[string]struct {
	schemaAccepts bool   // true: the schema accepts a config Load refuses
	doc           string // the bullet's wording in docs/configuration.md
}{
	// Rules JSON Schema cannot state: they relate one value to another
	// part of the config, to the environment or the file system, or to
	// Go's regex syntax and int size.
	"dependencies":   {true, "a `depends_on` entry that names an unknown process, the process itself or a teardown process, or that closes a cycle"},
	"upstream_port":  {true, "a merge upstream started with a `PORT` other than the merge port"},
	"client_process": {true, "a `merge.client_process` that names a task or teardown process"},
	"merge_port_env": {true, "an unset `merge.port` while the `PORT` environment variable is out of range"},
	"log_regex":      {true, "a `ready_when.log` that is not a valid Go regular expression"},
	"env_file":       {true, "an `env_file` that cannot be read or has a line longer than 1 MiB"},
	"label_bytes":    {true, "a resource label of at most 256 characters but more than 256 bytes"},
	"int_range":      {true, "an integer too large for the platform's `int`"},
	// Values the YAML decoder accepts that the schema's types refuse.
	"null":          {false, "a `null` value, which the loader reads as unset"},
	"empty_enum":    {false, "an empty `mode`, `restart`, `health_action` or `foreign_listener`, which the loader reads as the default"},
	"scalar_string": {false, "a number or boolean where a string is expected, which the loader reads as its text"},
	"fraction":      {false, "a fractional number where an integer is expected, which the loader truncates"},
}

// TestSchema_VerdictMatchesLoad pins the published schema to Load: for every
// sample config, the schema's verdict (schemaEvaluator) equals Load's,
// except for a sample that names one of the documented schemaDivergences —
// and there the two must really disagree, in the documented direction.
func TestSchema_VerdictMatchesLoad(t *testing.T) {
	proc := func(extra string) string { return "processes:\n  api:\n    cmd: x\n" + extra }
	resource := func(label string) string { return proc("resources:\n  - " + strconv.Quote(label) + "\n") }
	simple := "merge:\n  client: 3000\n  server: 3001\n"
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "environment-report", "proc-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	samples := []struct {
		name, yaml string
		envFile    string // written next to the config as .env
		portEnv    string // the PORT environment variable
		rule       string // a schemaDivergences key, or "" when the verdicts agree
	}{
		// Root: a non-empty processes map or a merge section.
		{name: "one process", yaml: proc("")},
		{name: "the repo's example config", yaml: string(example)},
		{name: "an empty document", yaml: ""},
		{name: "an empty processes map", yaml: "processes: {}\n"},
		{name: "an empty processes map with a merge section", yaml: "processes: {}\n" + simple},
		{name: "a merge section only", yaml: simple},
		{name: "neither processes nor merge", yaml: "resources: [db]\n"},
		{name: "an unknown root key", yaml: proc("bogus: 1\n")},
		{name: "an invalid process name", yaml: "processes:\n  -bad:\n    cmd: x\n"},

		// Process fields.
		{name: "no cmd", yaml: "processes:\n  api:\n    dir: .\n"},
		{name: "an empty cmd", yaml: "processes:\n  api:\n    cmd: \"\"\n"},
		{name: "an unknown restart policy", yaml: proc("    restart: sometimes\n")},
		{name: "an unknown mode", yaml: proc("    mode: daemon\n")},
		{name: "max_restarts with restart: never", yaml: proc("    restart: never\n    max_restarts: 3\n")},
		{name: "max_restarts with the default restart", yaml: proc("    max_restarts: 3\n")},
		{name: "max_restarts with restart: on-failure", yaml: proc("    restart: on-failure\n    max_restarts: 3\n")},
		{name: "a negative max_restarts", yaml: proc("    restart: always\n    max_restarts: -1\n")},
		{name: "a negative shutdown_timeout", yaml: proc("    shutdown_timeout: -1\n")},
		{name: "ready_timeout: -1", yaml: proc("    ready_timeout: -1\n")},
		{name: "ready_timeout: -2", yaml: proc("    ready_timeout: -2\n")},

		// Probes: exactly one non-empty kind; health_when has no log.
		{name: "an empty ready_when", yaml: proc("    ready_when: {}\n")},
		{name: "a ready_when with only an empty kind", yaml: proc("    ready_when:\n      http: \"\"\n")},
		{name: "a ready_when with an empty and a set kind", yaml: proc("    ready_when:\n      http: \"\"\n      tcp: localhost:1\n")},
		{name: "a ready_when with two kinds", yaml: proc("    ready_when:\n      http: http://x\n      tcp: localhost:1\n")},
		{name: "a ready_when log", yaml: proc("    ready_when:\n      log: ready\n")},
		{name: "a ready_when log that is not a regex", yaml: proc("    ready_when:\n      log: \"(\"\n"), rule: "log_regex"},
		{name: "a health_when log", yaml: proc("    health_when:\n      log: ready\n")},
		{name: "a health_when with an empty log", yaml: proc("    health_when:\n      log: \"\"\n      tcp: localhost:1\n")},
		{name: "health_interval without health_when", yaml: proc("    health_interval: 5\n")},
		{name: "health_interval: 0 without health_when", yaml: proc("    health_interval: 0\n")},
		{name: "health_action without health_when", yaml: proc("    health_action: report\n")},
		{name: "a negative health_interval", yaml: proc("    health_when:\n      tcp: localhost:1\n    health_interval: -1\n")},
		{name: "health_action: restart with restart: never", yaml: proc("    health_when:\n      tcp: localhost:1\n    health_action: restart\n")},
		{name: "health_action: restart with restart: always", yaml: proc("    restart: always\n    health_when:\n      tcp: localhost:1\n    health_action: restart\n")},
		{name: "foreign_listener with a tcp ready_when", yaml: proc("    foreign_listener: reject\n    ready_when:\n      tcp: localhost:1\n")},
		{name: "foreign_listener with an http health_when", yaml: proc("    foreign_listener: reject\n    health_when:\n      http: http://x\n")},
		{name: "foreign_listener with a cmd probe only", yaml: proc("    foreign_listener: reject\n    ready_when:\n      cmd: \"true\"\n")},
		{name: "foreign_listener with an empty http kind", yaml: proc("    foreign_listener: allow\n    ready_when:\n      http: \"\"\n      cmd: \"true\"\n")},
		{name: "an unknown foreign_listener", yaml: proc("    foreign_listener: maybe\n    ready_when:\n      tcp: localhost:1\n")},

		// Modes: a task or teardown process runs once.
		{name: "a task", yaml: proc("    mode: task\n    restart: never\n    ready_timeout: 0\n")},
		{name: "a task with ready_when", yaml: proc("    mode: task\n    ready_when:\n      log: done\n")},
		{name: "a task with ready_timeout", yaml: proc("    mode: task\n    ready_timeout: 5\n")},
		{name: "a task with restart: always", yaml: proc("    mode: task\n    restart: always\n")},
		{name: "a task with health_when", yaml: proc("    mode: task\n    health_when:\n      tcp: localhost:1\n")},
		{name: "a task with health_interval", yaml: proc("    mode: task\n    health_interval: 5\n")},
		{name: "a task with health_action", yaml: proc("    mode: task\n    health_action: report\n")},
		{name: "a task with foreign_listener", yaml: proc("    mode: task\n    foreign_listener: allow\n")},
		{name: "a task with ports", yaml: proc("    mode: task\n    ports:\n      http: 0\n")},
		{name: "a teardown process", yaml: proc("    mode: teardown\n    shutdown_timeout: 5\n    ports: {}\n    depends_on: []\n")},
		{name: "a teardown process with ports", yaml: proc("    mode: teardown\n    ports:\n      http: 0\n")},
		{name: "a teardown process with depends_on", yaml: proc("    mode: teardown\n    depends_on: [web]\n  web:\n    cmd: y\n")},
		{name: "a teardown process with a long shutdown_timeout", yaml: proc("    mode: teardown\n    shutdown_timeout: 6\n")},
		{name: "a teardown process with max_restarts", yaml: proc("    mode: teardown\n    restart: always\n    max_restarts: 1\n")},

		// Ports and dependencies.
		{name: "an ephemeral named port", yaml: proc("    ports:\n      http: 0\n")},
		{name: "an out-of-range named port", yaml: proc("    ports:\n      http: 70000\n")},
		{name: "a dotted port name", yaml: proc("    ports:\n      b.c: 0\n")},
		{name: "an unknown dependency", yaml: proc("    depends_on: [db]\n"), rule: "dependencies"},
		{name: "a self dependency", yaml: proc("    depends_on: [api]\n"), rule: "dependencies"},
		{name: "a dependency cycle", yaml: proc("    depends_on: [web]\n  web:\n    cmd: y\n    depends_on: [api]\n"), rule: "dependencies"},
		{name: "a teardown dependency", yaml: proc("    depends_on: [clean]\n  clean:\n    cmd: y\n    mode: teardown\n"), rule: "dependencies"},
		{name: "a readable env_file", yaml: proc("    env_file: .env\n"), envFile: "A=1\n"},
		{name: "a missing env_file", yaml: proc("    env_file: .env\n"), rule: "env_file"},
		{name: "an env_file line over 1 MiB", yaml: proc("    env_file: .env\n"), envFile: "A=" + strings.Repeat("x", 1<<20) + "\n", rule: "env_file"},

		// Resources: the label trimmed of surrounding whitespace is
		// non-empty, single-line, free of NUL and at most 256 bytes.
		{name: "a resource label", yaml: resource("db")},
		{name: "a padded resource label", yaml: resource(" db\n")},
		{name: "whitespace variants of a label", yaml: proc("resources: [db, \" db \"]\n")},
		{name: "a repeated resource label", yaml: proc("resources: [db, db]\n")},
		{name: "an empty resource label", yaml: resource("")},
		{name: "a whitespace-only resource label", yaml: resource(" ")},
		{name: "a tab and vertical tab resource label", yaml: resource("\t\v")},
		{name: "a no-break space resource label", yaml: resource(" 　")},
		{name: "a resource label with an inner line break", yaml: resource("a\nb")},
		{name: "a resource label with an inner carriage return", yaml: resource("a\rb")},
		{name: "a resource label with a NUL", yaml: resource("a\x00b")},
		{name: "a resource label that is only a NUL", yaml: resource("\x00")},
		{name: "a 256-character resource label", yaml: resource(strings.Repeat("a", 256))},
		{name: "a 257-character resource label", yaml: resource(strings.Repeat("a", 257))},
		{name: "a padded 256-character resource label", yaml: resource(" " + strings.Repeat("a", 256) + " ")},
		{name: "a 2-character resource label", yaml: resource("ab")},
		{name: "a 256-character resource label of 512 bytes", yaml: resource(strings.Repeat("é", 256)), rule: "label_bytes"},

		// Merge: the mode, the port ranges, the reserved name.
		{name: "the #21 regression config", yaml: "processes:\n  web:\n    cmd: node server.js\n    ports:\n      http: 3000\n    env:\n      PORT: \"${ports.web.http}\"\n  api:\n    cmd: node api.js\n    env:\n      PORT: \"4000\"\nmerge:\n  client: 3000\n  server: 4000\n"},
		{name: "an empty merge section", yaml: proc("merge: {}\n")},
		{name: "a merge client only", yaml: proc("merge:\n  client: 3000\n")},
		{name: "merge api_prefixes only", yaml: proc("merge:\n  api_prefixes: [/api]\n")},
		{name: "merge routes and a client", yaml: proc("merge:\n  routes: [/=3000]\n  client: 3000\n")},
		{name: "merge routes and api_prefixes", yaml: proc("merge:\n  routes: [/=3000]\n  api_prefixes: [/api]\n")},
		{name: "merge routes with zero client and empty prefixes", yaml: proc("merge:\n  routes: [/=3000]\n  client: 0\n  api_prefixes: []\n")},
		{name: "empty merge routes with a client and a server", yaml: proc("merge:\n  routes: []\n  client: 3000\n  server: 3001\n")},
		{name: "a merge client out of range", yaml: proc("merge:\n  client: 99999\n  server: 3001\n")},
		{name: "a negative merge client", yaml: proc("merge:\n  client: -5\n  server: 3001\n")},
		{name: "a merge server out of range", yaml: proc("merge:\n  client: 3000\n  server: 70000\n")},
		{name: "a merge port out of range", yaml: proc(simple + "  port: 70000\n")},
		{name: "a negative merge port", yaml: proc(simple + "  port: -1\n")},
		{name: "merge port 0", yaml: proc(simple + "  port: 0\n")},
		{name: "a route target out of range", yaml: proc("merge:\n  routes: [\"/api=99999\"]\n")},
		{name: "a route target of 0", yaml: proc("merge:\n  routes: [\"/api= 0 \"]\n")},
		{name: "a route target URL", yaml: proc("merge:\n  routes: [\"/api=http://api.local:3001\"]\n")},
		{name: "a merge-port process next to a merge section", yaml: simple + "processes:\n  merge-port:\n    cmd: my-proxy\n"},
		{name: "a merge-port process without a merge section", yaml: "processes:\n  merge-port:\n    cmd: my-proxy\n"},
		{name: "a name-matched upstream on another PORT", yaml: simple + "processes:\n  web:\n    cmd: x\n    env:\n      PORT: \"5173\"\n", rule: "upstream_port"},
		{name: "a client_process that names a task", yaml: simple + "  client_process: build\nprocesses:\n  build:\n    cmd: x\n    mode: task\n", rule: "client_process"},
		{name: "an unset merge port with PORT out of range", yaml: proc(simple), portEnv: "99999", rule: "merge_port_env"},

		// Values the YAML decoder reads more leniently than the schema types.
		{name: "a null ready_when", yaml: proc("    ready_when:\n"), rule: "null"},
		{name: "an empty restart", yaml: proc("    restart: \"\"\n"), rule: "empty_enum"},
		{name: "a numeric cmd", yaml: "processes:\n  api:\n    cmd: 123\n", rule: "scalar_string"},
		{name: "a fractional max_restarts", yaml: proc("    restart: always\n    max_restarts: 1.5\n"), rule: "fraction"},
		{name: "an integer beyond int64", yaml: proc("    shutdown_timeout: 99999999999999999999\n"), rule: "int_range"},
	}
	evaluator := newSchemaEvaluator(t)
	exercised := map[string]bool{}
	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			t.Setenv("PORT", sample.portEnv)
			path := writeConfig(t, sample.yaml)
			if sample.envFile != "" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte(sample.envFile), 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, loadErr := Load(path)
			var doc any
			if err := yaml.Unmarshal([]byte(sample.yaml), &doc); err != nil {
				t.Fatalf("sample is not YAML: %v", err)
			}
			schemaOK, loadOK := evaluator.valid(evaluator.root, doc), loadErr == nil
			if sample.rule == "" {
				if schemaOK != loadOK {
					t.Fatalf("schema accepts=%v, Load accepts=%v (err %v)", schemaOK, loadOK, loadErr)
				}
				return
			}
			divergence, ok := schemaDivergences[sample.rule]
			if !ok {
				t.Fatalf("rule %q is not a documented divergence", sample.rule)
			}
			exercised[sample.rule] = true
			if schemaOK != divergence.schemaAccepts || loadOK == divergence.schemaAccepts {
				t.Fatalf("divergence %q: schema accepts=%v, Load accepts=%v (err %v); want the schema to accept=%v and Load the opposite",
					sample.rule, schemaOK, loadOK, loadErr, divergence.schemaAccepts)
			}
		})
	}

	// Every documented divergence is exercised by a sample, and listed in
	// the configuration docs with the wording above.
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(docs)), " ")
	for rule, divergence := range schemaDivergences {
		if !exercised[rule] {
			t.Errorf("divergence %q is not exercised by any sample", rule)
		}
		if !strings.Contains(text, divergence.doc) {
			t.Errorf("docs/configuration.md does not list divergence %q: %q", rule, divergence.doc)
		}
	}
}

// TestSchema_SpaceClassMatchesTrimSpace pins the whitespace class the schema
// states the loader's trimming with: it matches exactly the runes
// strings.TrimSpace removes. The class is matched against the whole Basic
// Multilingual Plane; above it, TrimSpace must remove nothing, which the
// class (built from unicode.White_Space) agrees with by construction.
func TestSchema_SpaceClassMatchesTrimSpace(t *testing.T) {
	re := regexp.MustCompile("^[" + spaceClass + "]$")
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		s := string(r)
		trimmed := strings.TrimSpace(s) == ""
		if r > 0xFFFF {
			if trimmed || unicode.Is(unicode.White_Space, r) {
				t.Fatalf("rune %U above the BMP is whitespace; sweep the class over it too", r)
			}
			continue
		}
		if inClass := re.MatchString(s); inClass != trimmed {
			t.Fatalf("rune %U: in the schema's space class = %v, removed by TrimSpace = %v", r, inClass, trimmed)
		}
	}
}

// TestSchema_RouteTargetsMatchLoad pins the schema's route-target rule to
// checkRouteTarget for targets from -2 to 70000 — every 13th number (so every
// digit appears in every position) and a window around each boundary of the
// range pattern — written plainly, with a sign, with leading zeros and with
// padding.
func TestSchema_RouteTargetsMatchLoad(t *testing.T) {
	evaluator := newSchemaEvaluator(t)
	merge := evaluator.root["$defs"].(map[string]any)["Merge"].(map[string]any)
	items := merge["properties"].(map[string]any)["routes"].(map[string]any)["items"]
	var numbers []int
	for n := -2; n <= 70000; n += 13 {
		numbers = append(numbers, n)
	}
	for _, boundary := range []int{0, 10, 100, 1000, 10000, 60000, 65000, 65500, 65530, 65536, 70000, 99999} {
		for n := boundary - 3; n <= boundary+3; n++ {
			numbers = append(numbers, n)
		}
	}
	for _, n := range numbers {
		for _, target := range []string{strconv.Itoa(n), "+" + strconv.Itoa(n), "00" + strconv.Itoa(n), " " + strconv.Itoa(n) + "\t"} {
			route := "/api=" + target
			if schemaOK, loadOK := evaluator.valid(items, route), checkRouteTarget(0, route) == nil; schemaOK != loadOK {
				t.Fatalf("route %q: schema accepts=%v, Load accepts=%v", route, schemaOK, loadOK)
			}
		}
	}
}

// schemaEvaluator checks a document against the published schema. It
// implements the subset of JSON Schema draft 2020-12 that Schema() generates,
// with Go's regexp for "pattern", and fails the test on any other keyword, so
// a keyword added to the generator must be taught here before the parity
// tests can pass.
type schemaEvaluator struct {
	t        *testing.T
	root     map[string]any
	patterns map[string]*regexp.Regexp
}

func newSchemaEvaluator(t *testing.T) *schemaEvaluator {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	return &schemaEvaluator{t: t, root: root, patterns: map[string]*regexp.Regexp{}}
}

func (e *schemaEvaluator) pattern(expr string) *regexp.Regexp {
	re, ok := e.patterns[expr]
	if !ok {
		re = regexp.MustCompile(expr)
		e.patterns[expr] = re
	}
	return re
}

func (e *schemaEvaluator) valid(schema, value any) bool {
	e.t.Helper()
	if accept, ok := schema.(bool); ok {
		return accept
	}
	s, ok := schema.(map[string]any)
	if !ok {
		e.t.Fatalf("schema node is a %T", schema)
	}
	obj, isObject := value.(map[string]any)
	arr, isArray := value.([]any)
	str, isString := value.(string)
	num, isNumber := schemaNumber(value)
	for _, key := range SortedProcessNames(s) {
		arg := s[key]
		ok := true
		switch key {
		case "$schema", "$id", "title", "$defs", "default", "then", "else":
			// Annotations; then/else are applied by "if".
		case "$ref":
			ok = e.valid(e.root["$defs"].(map[string]any)[strings.TrimPrefix(arg.(string), "#/$defs/")], value)
		case "type":
			ok = schemaHasType(value, arg.(string))
		case "properties":
			for name, sub := range arg.(map[string]any) {
				if v, present := obj[name]; isObject && present && !e.valid(sub, v) {
					ok = false
				}
			}
		case "additionalProperties":
			declared, _ := s["properties"].(map[string]any)
			for name, v := range obj {
				if _, known := declared[name]; !known && !e.valid(arg, v) {
					ok = false
				}
			}
		case "required":
			for _, name := range arg.([]any) {
				if _, present := obj[name.(string)]; isObject && !present {
					ok = false
				}
			}
		case "propertyNames":
			for name := range obj {
				ok = ok && e.valid(arg, name)
			}
		case "minProperties":
			ok = !isObject || float64(len(obj)) >= arg.(float64)
		case "maxProperties":
			ok = !isObject || float64(len(obj)) <= arg.(float64)
		case "dependentSchemas":
			for name, sub := range arg.(map[string]any) {
				if _, present := obj[name]; isObject && present && !e.valid(sub, value) {
					ok = false
				}
			}
		case "items":
			for _, item := range arr {
				ok = ok && e.valid(arg, item)
			}
		case "minItems":
			ok = !isArray || float64(len(arr)) >= arg.(float64)
		case "maxItems":
			ok = !isArray || float64(len(arr)) <= arg.(float64)
		case "uniqueItems":
			seen := map[string]bool{}
			for _, item := range arr {
				key := schemaCanonical(e.t, item)
				ok = ok && !(arg.(bool) && seen[key])
				seen[key] = true
			}
		case "minLength":
			ok = !isString || float64(utf8.RuneCountInString(str)) >= arg.(float64)
		case "pattern":
			ok = !isString || e.pattern(arg.(string)).MatchString(str)
		case "enum":
			ok = false
			for _, allowed := range arg.([]any) {
				ok = ok || schemaCanonical(e.t, allowed) == schemaCanonical(e.t, value)
			}
		case "const":
			ok = schemaCanonical(e.t, arg) == schemaCanonical(e.t, value)
		case "minimum":
			ok = !isNumber || num >= arg.(float64)
		case "maximum":
			ok = !isNumber || num <= arg.(float64)
		case "anyOf", "oneOf":
			matched := 0
			for _, sub := range arg.([]any) {
				if e.valid(sub, value) {
					matched++
				}
			}
			ok = matched > 0 && (key == "anyOf" || matched == 1)
		case "allOf":
			for _, sub := range arg.([]any) {
				ok = ok && e.valid(sub, value)
			}
		case "not":
			ok = !e.valid(arg, value)
		case "if":
			branch := "else"
			if e.valid(arg, value) {
				branch = "then"
			}
			if sub, present := s[branch]; present {
				ok = e.valid(sub, value)
			}
		default:
			e.t.Fatalf("the schema evaluator does not implement keyword %q", key)
		}
		if !ok {
			return false
		}
	}
	return true
}

// schemaNumber reports a decoded YAML number as a float64.
func schemaNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func schemaHasType(value any, typ string) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		n, ok := schemaNumber(value)
		return ok && n == math.Trunc(n)
	}
	panic("schema type " + typ)
}

// schemaCanonical renders a value for JSON Schema equality: numbers compare
// by value and objects regardless of key order.
func schemaCanonical(t *testing.T, value any) string {
	if n, ok := schemaNumber(value); ok {
		return strconv.FormatFloat(n, 'g', -1, 64)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
