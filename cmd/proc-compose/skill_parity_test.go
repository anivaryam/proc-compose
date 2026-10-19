package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/anivaryam/proc-compose/internal/config"
	"gopkg.in/yaml.v3"
)

// The agent skill is Markdown that users copy or link into ~/.claude/skills,
// outside this repository, so it cannot share code with the CLI. These tests
// keep it from drifting: it must cover every command and config field, show
// only commands that exist, and load in both Claude Code and OpenCode.

var skillDir = filepath.Join("..", "..", "skills", "proc-compose")

type skillLine struct {
	text   string
	number int
	fenced bool // inside a ``` code block
}

type skillFile struct {
	path  string
	text  string
	lines []skillLine
}

func readSkillFiles(t *testing.T) []skillFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(skillDir, "references", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append([]string{filepath.Join(skillDir, "SKILL.md")}, paths...)
	files := make([]skillFile, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read skill file: %v", err)
		}
		f := skillFile{path: path, text: strings.ReplaceAll(string(data), "\r\n", "\n")}
		fenced := false
		for i, line := range strings.Split(f.text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenced = !fenced
				continue
			}
			f.lines = append(f.lines, skillLine{text: line, number: i + 1, fenced: fenced})
		}
		files = append(files, f)
	}
	return files
}

func skillText(files []skillFile) string {
	var all strings.Builder
	for _, f := range files {
		all.WriteString(f.text)
		all.WriteString("\n")
	}
	return all.String()
}

// cliCommandPaths lists the built binary's commands from its help output, as
// "up" or "config check". Cobra's generated help command and the completion
// shells are not proc-compose features.
func cliCommandPaths(t *testing.T, bin string, parent ...string) []string {
	t.Helper()
	out, err := exec.Command(bin, append(slices.Clone(parent), "--help")...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v --help: %v\n%s", parent, err, out)
	}
	var paths []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	listing := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "Available Commands:" {
			listing = true
			continue
		}
		if !listing {
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		name := strings.Fields(line)[0]
		if name == "help" {
			continue
		}
		path := append(slices.Clone(parent), name)
		paths = append(paths, strings.Join(path, " "))
		if len(parent) == 0 && name != "completion" {
			paths = append(paths, cliCommandPaths(t, bin, name)...)
		}
	}
	return paths
}

func TestSkill_FrontmatterLoadsInClaudeCodeAndOpenCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "---\n")
	if !ok {
		t.Fatal("SKILL.md must start with YAML frontmatter")
	}
	block, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	var front map[string]any
	if err := yaml.Unmarshal([]byte(block), &front); err != nil {
		t.Fatalf("frontmatter: %v", err)
	}
	// OpenCode documents only these keys; Claude-only keys such as
	// allowed-tools are not promised to load there.
	portable := []string{"name", "description", "license", "compatibility", "metadata"}
	for key := range front {
		if !slices.Contains(portable, key) {
			t.Errorf("frontmatter key %q is not portable to OpenCode", key)
		}
	}
	name, _ := front["name"].(string)
	if name != filepath.Base(skillDir) || len(name) > 64 || !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(name) {
		t.Errorf("name %q must match the directory and ^[a-z0-9]+(-[a-z0-9]+)*$ (at most 64 characters)", name)
	}
	description, _ := front["description"].(string)
	if n := len([]rune(description)); n == 0 || n > 1024 {
		t.Errorf("description has %d characters, want 1..1024", n)
	}
}

// Where a proc-compose command begins: at the start of a code line or after a
// shell separator, or at the start of inline code. "brokit install
// proc-compose" is not a proc-compose command.
var (
	fencedCommand = regexp.MustCompile(`(?:^|^\$ |run: |&& |; |\| )proc-compose (.*)`)
	inlineCommand = regexp.MustCompile("`proc-compose ([^`]*)`")
)

func TestSkill_CommandsMatchCLI(t *testing.T) {
	files := readSkillFiles(t)
	paths := cliCommandPaths(t, buildTestBinary(t))
	if !slices.Contains(paths, "verify") || !slices.Contains(paths, "config check") {
		t.Fatalf("help parsing found %v", paths)
	}

	text := skillText(files)
	known := map[string]bool{"--version": true, "--help": true}
	for _, path := range paths {
		known[strings.Fields(path)[0]] = true
		if !regexp.MustCompile("proc-compose " + regexp.QuoteMeta(path) + "([\\s`]|$)").MatchString(text) {
			t.Errorf("skill never shows `proc-compose %s`", path)
		}
	}

	for _, f := range files {
		for _, line := range f.lines {
			pattern := inlineCommand
			if line.fenced {
				pattern = fencedCommand
			}
			for _, match := range pattern.FindAllStringSubmatch(strings.TrimSpace(line.text), -1) {
				args := strings.Fields(match[1])
				for len(args) > 0 && slices.Contains([]string{"-f", "--file", "--session"}, args[0]) {
					args = args[min(2, len(args)):]
				}
				if len(args) > 0 && !strings.HasPrefix(args[0], "<") && !known[args[0]] {
					t.Errorf("%s:%d shows unknown command %q", f.path, line.number, args[0])
				}
			}
		}
	}
}

func TestSkill_CoversEveryConfigField(t *testing.T) {
	text := skillText(readSkillFiles(t))
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(config.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]bool)
	for name := range schema.Properties {
		fields[name] = true
	}
	for _, def := range schema.Defs {
		for name := range def.Properties {
			fields[name] = true
		}
	}
	for field := range fields {
		if !regexp.MustCompile(`(^|[\s{,])` + regexp.QuoteMeta(field) + `:`).MatchString(text) {
			t.Errorf("skill never shows config field %q as YAML", field)
		}
	}
}

var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// headingAnchor is GitHub's anchor for a Markdown heading.
func headingAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestSkill_LinksResolve(t *testing.T) {
	files := readSkillFiles(t)
	anchors := make(map[string][]string)
	for _, f := range files {
		for _, line := range f.lines {
			if !line.fenced && strings.HasPrefix(line.text, "#") {
				anchors[f.path] = append(anchors[f.path], headingAnchor(strings.TrimLeft(line.text, "# ")))
			}
		}
	}
	for _, f := range files {
		for _, match := range markdownLink.FindAllStringSubmatch(f.text, -1) {
			target := match[1]
			if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			path := f.path
			if file != "" {
				path = filepath.Join(filepath.Dir(f.path), filepath.FromSlash(file))
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s links to missing %s", f.path, target)
					continue
				}
			}
			if anchor != "" && !slices.Contains(anchors[path], anchor) {
				t.Errorf("%s links to missing heading %s", f.path, target)
			}
		}
	}
}
