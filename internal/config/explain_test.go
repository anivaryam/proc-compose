package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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

// TestDiagnosticHints_NameTheRuleTheyExplain pins the two hints that state a
// rule: the invalid_merge hint names the refused name-matched PORT, and the
// invalid_port hint says that a dot is not allowed in a port name.
func TestDiagnosticHints_NameTheRuleTheyExplain(t *testing.T) {
	if !strings.Contains(diagnosticHints[CodeInvalidMerge], "PORT") ||
		!strings.Contains(diagnosticHints[CodeInvalidMerge], "merge port") {
		t.Fatalf("invalid_merge hint omits the refused PORT rule: %q", diagnosticHints[CodeInvalidMerge])
	}
	if !strings.Contains(diagnosticHints[CodeInvalidPort], "`.` is not allowed in port names") {
		t.Fatalf("invalid_port hint does not say that a dot is not allowed: %q", diagnosticHints[CodeInvalidPort])
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

// ─── #56: the published schema's root constraint ──────────────────────────────

// TestSchema_RootAnyOfRequiresANonEmptyProcessesMapOrAMerge pins the root
// anyOf of the published schema: a config carries a non-empty processes map or
// a merge section, exactly as Load requires. The measurement is the generated
// keywords plus the verdict of the changed constraint for a table of sample
// documents.
func TestSchema_RootAnyOfRequiresANonEmptyProcessesMapOrAMerge(t *testing.T) {
	var schema struct {
		AnyOf []map[string]any `json:"anyOf"`
	}
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("root anyOf has %d branches, want 2", len(schema.AnyOf))
	}
	first := schema.AnyOf[0]
	required, _ := first["required"].([]any)
	if len(required) != 1 || required[0] != "processes" {
		t.Fatalf("branch 1 required = %v, want [processes]", required)
	}
	props, _ := first["properties"].(map[string]any)
	processes, _ := props["processes"].(map[string]any)
	min, ok := processes["minProperties"].(float64)
	if !ok || min != 1 {
		t.Fatalf("branch 1 properties.processes.minProperties = %v, want 1", processes["minProperties"])
	}
	second := schema.AnyOf[1]
	secondRequired, _ := second["required"].([]any)
	if len(secondRequired) != 1 || secondRequired[0] != "merge" {
		t.Fatalf("branch 2 required = %v, want [merge]", secondRequired)
	}

	// The changed constraint, evaluated against sample documents. Only the
	// root anyOf is evaluated: it is the constraint this change adds. Each
	// row below is therefore a verdict about the root anyOf branch, not about
	// the whole schema — the real schema also requires `processes` to be an
	// object, which this evaluator does not model.
	samples := []struct {
		name      string
		yaml      string
		wantValid bool
	}{
		{"root anyOf verdict: an empty processes map without a merge section", "processes: {}\n", false},
		{"root anyOf verdict: an empty processes map with a merge section", "processes: {}\nmerge:\n  client: 3000\n  server: 3001\n", true},
		{"root anyOf verdict: a null processes map without a merge section", "processes:\n", false},
		{"root anyOf verdict: a null processes map with a merge section", "processes:\nmerge:\n  client: 3000\n  server: 3001\n", true},
		{"root anyOf verdict: one process", "processes:\n  api:\n    cmd: x\n", true},
		{"root anyOf verdict: a merge section only", "merge:\n  client: 3000\n  server: 3001\n", true},
		{"root anyOf verdict: neither a processes map nor a merge section", "resources: [db]\n", false},
	}
	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(sample.yaml), &doc); err != nil {
				t.Fatal(err)
			}
			if got := rootAnyOfAccepts(doc, schema.AnyOf); got != sample.wantValid {
				t.Fatalf("root anyOf verdict = %v, want %v", got, sample.wantValid)
			}
		})
	}

	// The repo's own example config stays valid under the changed
	// constraint, and Load still accepts it.
	example := filepath.Join("..", "..", "examples", "environment-report", "proc-compose.yml")
	data, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !rootAnyOfAccepts(doc, schema.AnyOf) {
		t.Fatal("the repo's example config is invalid under the changed constraint")
	}
	if _, err := Load(example); err != nil {
		t.Fatalf("Load(%s): %v", example, err)
	}
}

// rootAnyOfAccepts evaluates the root anyOf of the published schema against a
// parsed document: a branch is satisfied when it requires the keys it names
// and the properties it constrains hold.
func rootAnyOfAccepts(doc map[string]any, branches []map[string]any) bool {
	for _, branch := range branches {
		satisfied := true
		if required, ok := branch["required"].([]any); ok {
			for _, key := range required {
				name, _ := key.(string)
				if _, present := doc[name]; !present {
					satisfied = false
					break
				}
			}
		}
		if !satisfied {
			continue
		}
		if props, ok := branch["properties"].(map[string]any); ok {
			for name, constraint := range props {
				value, present := doc[name]
				if !present {
					continue
				}
				rule, _ := constraint.(map[string]any)
				min, ok := rule["minProperties"].(float64)
				if !ok {
					continue
				}
				entries, isMap := value.(map[string]any)
				if !isMap || float64(len(entries)) < min {
					satisfied = false
					break
				}
			}
		}
		if satisfied {
			return true
		}
	}
	return false
}
