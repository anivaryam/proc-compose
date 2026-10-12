package config

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
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
		{"invalid port", "processes:\n  api:\n    cmd: x\n    ports:\n      http: 70000\n", CodeInvalidPort, "processes.api.ports.http", 5},
		{"invalid ready_when", "processes:\n  api:\n    cmd: x\n    ready_when:\n      http: http://x\n      tcp: x:1\n", CodeInvalidReadyWhen, "processes.api.ready_when", 5},
		{"invalid resource", "processes:\n  api:\n    cmd: x\nresources: [db, db]\n", CodeInvalidResource, "resources[1]", 4},
		{"invalid merge", "merge:\n  routes: [\"/api=1\"]\n  client: 3000\n", CodeInvalidMerge, "merge", 2},
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

	all, err := Plan(cfg, nil)
	if err != nil || len(all.Processes) != 4 || !reflect.DeepEqual(all.Levels[0], []string{"cache", "db"}) {
		t.Fatalf("full plan = %+v, %v", all, err)
	}
	if _, err := Plan(cfg, []string{"nope"}); err == nil {
		t.Fatal("unknown filter accepted")
	}
}
