package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var safeProcessNameRe = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9._]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)

type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	Dependencies   map[string]string `json:"dependencies"`
	DevDeps        map[string]string `json:"devDependencies"`
	PackageManager string            `json:"packageManager"`
	Workspaces     json.RawMessage   `json:"workspaces"`
}

func Run(opts Options) (*Report, error) {
	report := &Report{
		Services: []Service{},
		Findings: []Finding{},
	}

	root := opts.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root = absRoot

	if opts.ConfigFile != "" {
		if !filepath.IsAbs(opts.ConfigFile) {
			report.ConfigPath = filepath.Join(root, opts.ConfigFile)
		} else {
			report.ConfigPath = opts.ConfigFile
		}
		if _, err := os.Stat(report.ConfigPath); err == nil {
			report.ConfigExists = true
		}
	} else {
		for _, name := range []string{"proc-compose.yml", "proc-compose.yaml"} {
			p := filepath.Join(root, name)
			if _, err := os.Stat(p); err == nil {
				report.ConfigPath = p
				report.ConfigExists = true
				break
			}
		}
		if report.ConfigPath == "" {
			report.ConfigPath = filepath.Join(root, "proc-compose.yml")
		}
	}

	if err := scanNodeServices(root, report); err != nil {
		return nil, err
	}

	if err := scanGoServices(root, report); err != nil {
		return nil, err
	}

	if err := scanPythonServices(root, report); err != nil {
		return nil, err
	}

	sort.Slice(report.Services, func(i, j int) bool {
		if report.Services[i].Dir != report.Services[j].Dir {
			return report.Services[i].Dir < report.Services[j].Dir
		}
		return report.Services[i].Name < report.Services[j].Name
	})
	report.Services = withUniqueServiceNames(report.Services)

	if len(report.Services) == 0 && !report.ConfigExists {
		report.Findings = append(report.Findings, Finding{
			Severity:   SeverityWarning,
			Code:       "no-services",
			Message:    "No services detected and no proc-compose.yml found",
			Suggestion: "Run 'proc-compose init' or create a proc-compose.yml file",
		})
	}
	if len(report.Services) > 0 {
		report.SuggestedYAML = renderSuggestedYAML(report.Services)
	}
	if report.ConfigExists {
		checkExistingConfig(report, root)
	}
	if opts.Write {
		if report.ConfigExists {
			return report, fmt.Errorf("%s already exists", report.ConfigPath)
		}
		if strings.TrimSpace(report.SuggestedYAML) == "" {
			return report, fmt.Errorf("no services detected; refusing to write empty proc-compose.yml")
		}
		file, err := os.OpenFile(report.ConfigPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return report, err
		}
		if _, err := io.WriteString(file, report.SuggestedYAML); err != nil {
			_ = file.Close()
			return report, err
		}
		if err := file.Close(); err != nil {
			return report, err
		}
		report.GeneratedPath = report.ConfigPath
	}

	return report, nil
}

// scanNodeServices finds runnable package.json projects under root and records
// them as Node services.
//
// Workflow:
//  1. Walk root while skipping generated, cache, vendored, and archive dirs.
//  2. Read each package.json and skip aggregate workspace/orchestrator packages
//     unless their dev/start script directly starts a known dev server.
//  3. Pick the package-manager-aware dev/start command, infer a port, and append
//     a high-confidence node service.
//
// Edge cases:
//   - Unreadable or invalid package.json files are ignored.
//   - Packages without dev/start scripts are ignored.
//   - Root packages use dir "." and manifest "package.json".
func scanNodeServices(root string, report *Report) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && shouldSkipProjectDir(path, d.Name()) {
			return filepath.SkipDir
		}
		pkgJSONPath := filepath.Join(path, "package.json")
		if _, err := os.Stat(pkgJSONPath); err != nil {
			return nil
		}

		pkg, err := readPackageJSON(pkgJSONPath)
		if err != nil {
			return nil
		}
		if isAggregateNodePackage(pkg) && !hasDirectNodeDevServer(pkg) {
			return nil
		}

		script := detectNodeCommand(root, path, pkg)
		if script == "" {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		port := detectNodePort(path, pkg)

		name := filepath.Base(path)
		dir := "."
		manifest := "package.json"
		if rel != "." {
			dir = "./" + rel
			manifest = "./" + rel + "/package.json"
		}

		report.Services = append(report.Services, Service{
			Name:       sanitizeProcessName(name),
			Command:    script,
			Dir:        dir,
			Port:       port,
			Kind:       "node",
			Confidence: ConfidenceHigh,
			Manifest:   manifest,
		})
		return nil
	})
}

func readPackageJSON(path string) (*packageJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

// detectNodePort returns the best port guess for a Node package.
//
// Workflow:
//  1. Prefer PORT from .env.example, .env, and .env.* files.
//  2. Parse --port/-p flags from dev/start scripts.
//  3. Fall back to framework defaults for Astro, Vite/SvelteKit, and Next.
//
// Edge cases:
//   - Missing env files, scripts, dependencies, or unknown frameworks return 0.
//   - Vitest dependency names are not treated as Vite dev-server evidence.
func detectNodePort(dir string, pkg *packageJSON) int {
	if port := detectEnvPort(dir); port > 0 {
		return port
	}
	for _, name := range []string{"dev", "start"} {
		if port := detectPortArg(pkg.Scripts[name]); port > 0 {
			return port
		}
	}

	hasVite := false
	hasNext := false
	hasSvelteKit := false
	hasAstro := false
	for _, name := range []string{"dev", "start"} {
		command := pkg.Scripts[name]
		fields := strings.Fields(command)
		for _, field := range fields {
			if field == "vite" {
				hasVite = true
			}
			if field == "next" {
				hasNext = true
			}
			if field == "svelte-kit" {
				hasSvelteKit = true
			}
			if field == "astro" {
				hasAstro = true
			}
		}
	}

	if pkg.Dependencies != nil {
		if _, ok := pkg.Dependencies["vite"]; ok {
			hasVite = true
		}
		if _, ok := pkg.Dependencies["next"]; ok {
			hasNext = true
		}
		if _, ok := pkg.Dependencies["@sveltejs/kit"]; ok {
			hasSvelteKit = true
		}
		if _, ok := pkg.Dependencies["astro"]; ok {
			hasAstro = true
		}
	}
	if pkg.DevDeps != nil {
		if _, ok := pkg.DevDeps["vite"]; ok {
			hasVite = true
		}
		if _, ok := pkg.DevDeps["next"]; ok {
			hasNext = true
		}
		if _, ok := pkg.DevDeps["@sveltejs/kit"]; ok {
			hasSvelteKit = true
		}
		if _, ok := pkg.DevDeps["astro"]; ok {
			hasAstro = true
		}
	}

	if hasAstro {
		return 4321
	}
	if hasVite {
		return 5173
	}
	if hasSvelteKit {
		return 5173
	}
	if hasNext {
		return 3000
	}
	return 0
}

// detectNodeCommand returns the command users should run for a Node service.
// It picks dev before start and chooses npm/yarn/pnpm/bun syntax from the
// packageManager field or lockfiles. Packages without runnable scripts return
// an empty string.
func detectNodeCommand(root, dir string, pkg *packageJSON) string {
	manager := detectNodePackageManager(root, dir, pkg)
	for _, scriptName := range []string{"dev", "start"} {
		if _, ok := pkg.Scripts[scriptName]; !ok {
			continue
		}
		switch manager {
		case "yarn", "pnpm", "bun":
			return manager + " " + scriptName
		default:
			return "npm run " + scriptName
		}
	}
	return ""
}

// detectNodePackageManager infers the package runner for a package directory.
// The packageManager field wins over lockfiles; lockfiles prefer pnpm, yarn,
// bun, then npm. Unknown or missing metadata falls back to npm.
func detectNodePackageManager(root, dir string, pkg *packageJSON) string {
	if pkg.PackageManager != "" {
		name := strings.SplitN(pkg.PackageManager, "@", 2)[0]
		switch name {
		case "npm", "pnpm", "yarn", "bun":
			return name
		}
	}
	root = filepath.Clean(root)
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		for _, candidate := range []struct{ file, manager string }{
			{"pnpm-lock.yaml", "pnpm"},
			{"yarn.lock", "yarn"},
			{"bun.lockb", "bun"},
			{"bun.lock", "bun"},
			{"package-lock.json", "npm"},
		} {
			if _, err := os.Stat(filepath.Join(current, candidate.file)); err == nil {
				return candidate.manager
			}
		}
		if current == root || current == filepath.Dir(current) {
			break
		}
	}
	return "npm"
}

// detectPortArg extracts --port/-p values from a package script. It accepts
// split and equals forms, ignores malformed values, and returns 0 when no valid
// positive port is present.
func detectPortArg(command string) int {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--port" || field == "-p" {
			if i+1 < len(fields) {
				if port, err := strconv.Atoi(strings.Trim(fields[i+1], `"'`)); err == nil && port > 0 {
					return port
				}
			}
			continue
		}
		for _, prefix := range []string{"--port=", "-p="} {
			if strings.HasPrefix(field, prefix) {
				if port, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(field, prefix), `"'`)); err == nil && port > 0 {
					return port
				}
			}
		}
	}
	return 0
}

// hasDirectNodeDevServer distinguishes runnable workspace roots from aggregate
// package.json files. Known direct dev-server commands are kept as services;
// orchestrator-only workspace roots remain skipped.
func hasDirectNodeDevServer(pkg *packageJSON) bool {
	for _, command := range []string{pkg.Scripts["dev"], pkg.Scripts["start"]} {
		fields := strings.Fields(command)
		for _, field := range fields {
			switch field {
			case "vite", "next", "astro", "svelte-kit":
				return true
			}
		}
	}
	return false
}

func isAggregateNodePackage(pkg *packageJSON) bool {
	if len(pkg.Workspaces) > 0 && string(pkg.Workspaces) != "null" {
		return true
	}
	if pkg.Scripts != nil && strings.Contains(pkg.Scripts["dev"], "concurrently") {
		return true
	}
	return false
}

// detectEnvPort reads PORT from common dotenv files in a project directory.
//
// Workflow:
//  1. Check .env.example and .env first for stable sample/default values.
//  2. Check other .env.* files deterministically.
//  3. Accept PORT=value and export PORT=value lines with quoted or unquoted
//     positive integer values.
//
// Edge cases:
//   - Missing files, comments, malformed assignments, and non-numeric ports are
//     ignored.
func detectEnvPort(dir string) int {
	filenames := []string{".env.example", ".env"}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.Type().IsRegular() && strings.HasPrefix(name, ".env.") && name != ".env.example" {
				filenames = append(filenames, name)
			}
		}
	}
	sort.Strings(filenames[2:])
	for _, filename := range filenames {
		envPath := filepath.Join(dir, filename)
		content, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}

		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "export ")
			if strings.HasPrefix(line, "PORT") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) != 2 || strings.TrimSpace(parts[0]) != "PORT" {
					continue
				}
				portStr := strings.TrimSpace(parts[1])
				portStr = strings.Trim(portStr, `"'`)
				if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
					return p
				}
			}
		}
	}
	return 0
}

// scanGoServices finds Go binaries in a module root.
//
// Workflow:
//  1. Require a root go.mod.
//  2. Walk cmd/** for directories containing package-main main.go files.
//  3. Generate root-relative go run ./cmd/... commands so binaries run from
//     the module root.
//  4. If no cmd binary exists, fall back to root main.go.
//
// Edge cases:
//   - Unsafe command directory names are skipped.
//   - Nested generated/vendor/cache dirs under cmd are skipped.
//   - Port is inferred from the binary dir first, then root dotenv files.
func scanGoServices(root string, report *Report) error {
	goModPath := filepath.Join(root, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		return nil
	}

	cmdDir := filepath.Join(root, "cmd")
	if _, err := os.Stat(cmdDir); err != nil {
		addRootGoService(root, report)
		return nil
	}

	found := false
	if err := filepath.WalkDir(cmdDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != cmdDir && shouldSkipProjectDir(path, d.Name()) {
			return filepath.SkipDir
		}
		mainPath := filepath.Join(path, "main.go")
		if !isGoMain(mainPath) {
			return nil
		}
		name := filepath.Base(path)
		if !safeProcessNameRe.MatchString(name) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if !safeRelativeCommandPath(rel) {
			return nil
		}
		port := detectEnvPort(path)
		if port == 0 {
			port = detectEnvPort(root)
		}

		report.Services = append(report.Services, Service{
			Name:       sanitizeProcessName(name),
			Command:    "go run ./" + rel,
			Dir:        ".",
			Port:       port,
			Kind:       "go",
			Confidence: ConfidenceMedium,
			Manifest:   "go.mod",
		})
		found = true
		return nil
	}); err != nil {
		return err
	}
	if !found {
		addRootGoService(root, report)
	}

	return nil
}

func addRootGoService(root string, report *Report) {
	if !isGoMain(filepath.Join(root, "main.go")) {
		return
	}
	report.Services = append(report.Services, Service{
		Name:       sanitizeProcessName(filepath.Base(root)),
		Command:    "go run .",
		Dir:        ".",
		Port:       detectEnvPort(root),
		Kind:       "go",
		Confidence: ConfidenceMedium,
		Manifest:   "go.mod",
	})
}

func isGoMain(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "package main")
}

func safeRelativeCommandPath(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if !safeProcessNameRe.MatchString(segment) {
			return false
		}
	}
	return true
}

func scanPythonServices(root string, report *Report) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if shouldSkipProjectDir(path, d.Name()) {
			return filepath.SkipDir
		}

		command, manifest, port, confidence := detectPythonCommand(path)
		if command == "" {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		dir := "."
		manifestPath := manifest
		if rel != "." {
			dir = "./" + rel
			manifestPath = "./" + rel + "/" + manifest
		}

		name := filepath.Base(path)
		if rel == "." {
			name = filepath.Base(root)
		}
		report.Services = append(report.Services, Service{
			Name:       sanitizeProcessName(name),
			Command:    command,
			Dir:        dir,
			Port:       port,
			Kind:       "python",
			Confidence: confidence,
			Manifest:   manifestPath,
		})
		return nil
	})
}

func sanitizeProcessName(name string) string {
	name = strings.TrimSpace(name)
	if safeProcessNameRe.MatchString(name) {
		return name
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	cleaned := strings.Trim(b.String(), "-._")
	if cleaned == "" {
		return "service"
	}
	if !safeProcessNameRe.MatchString(cleaned) {
		return "service"
	}
	return cleaned
}

func withUniqueServiceNames(services []Service) []Service {
	names := uniqueProcessNames(services)
	for i := range services {
		services[i].Name = names[i]
	}
	return services
}

func shouldSkipProjectDir(path, name string) bool {
	if name == "." || name == "" {
		return false
	}
	if strings.HasPrefix(name, ".") && name != "." {
		return true
	}
	switch name {
	case "node_modules", "vendor", "dist", "build", "coverage", "__pycache__", ".cache", ".turbo", ".worktrees", "archive", "venv", ".venv", "tmp", "temp":
		return true
	}
	return strings.Contains(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator))
}

// detectPythonCommand returns a conservative run command for a Python project.
//
// Workflow:
//  1. Prefer Django manage.py and default to port 8000.
//  2. For app.py/main.py with manifests, inspect dependency text for Flask,
//     Uvicorn/FastAPI, or Hypercorn and generate the matching CLI command.
//  3. Prefix with poetry run or pipenv run when lock/manifest files indicate
//     those environments.
//  4. Fall back to python app.py/main.py for generic Python apps.
//
// Edge cases:
//   - Non-manifest app.py/main.py directories are ignored.
//   - Missing PORT uses framework defaults: Flask 5000, ASGI/Django 8000.
//   - Unknown manifests produce medium-confidence generic commands.
func detectPythonCommand(dir string) (string, string, int, Confidence) {
	port := detectEnvPort(dir)
	if _, err := os.Stat(filepath.Join(dir, "manage.py")); err == nil {
		if port == 0 {
			port = 8000
		}
		return fmt.Sprintf("python manage.py runserver 0.0.0.0:%d", port), "manage.py", port, ConfidenceHigh
	}
	manifestText := pythonManifestText(dir)
	runner := pythonRunner(dir)
	if _, err := os.Stat(filepath.Join(dir, "app.py")); err == nil && hasPythonManifest(dir) {
		if strings.Contains(manifestText, "flask") {
			if port == 0 {
				port = 5000
			}
			return runner + "python -m flask --app app run --host 0.0.0.0 --port " + strconv.Itoa(port), "app.py", port, ConfidenceHigh
		}
		return runner + "python app.py", "app.py", port, ConfidenceMedium
	}
	if _, err := os.Stat(filepath.Join(dir, "main.py")); err == nil && hasPythonManifest(dir) {
		if strings.Contains(manifestText, "uvicorn") || strings.Contains(manifestText, "fastapi") || pythonFileContains(filepath.Join(dir, "main.py"), "FastAPI") {
			if port == 0 {
				port = 8000
			}
			return runner + "python -m uvicorn main:app --host 0.0.0.0 --port " + strconv.Itoa(port) + " --reload", "main.py", port, ConfidenceHigh
		}
		if strings.Contains(manifestText, "hypercorn") {
			if port == 0 {
				port = 8000
			}
			return runner + "python -m hypercorn main:app --bind 0.0.0.0:" + strconv.Itoa(port) + " --reload", "main.py", port, ConfidenceHigh
		}
		if strings.Contains(manifestText, "flask") {
			if port == 0 {
				port = 5000
			}
			return runner + "python -m flask --app main run --host 0.0.0.0 --port " + strconv.Itoa(port), "main.py", port, ConfidenceHigh
		}
		return runner + "python main.py", "main.py", port, ConfidenceMedium
	}
	return "", "", 0, ConfidenceLow
}

// pythonRunner returns the environment runner prefix for Poetry or Pipenv
// projects. Projects without those files run Python directly.
func pythonRunner(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "poetry.lock")); err == nil {
		return "poetry run "
	}
	if _, err := os.Stat(filepath.Join(dir, "Pipfile")); err == nil {
		return "pipenv run "
	}
	return ""
}

// pythonManifestText concatenates lower-cased Python dependency manifests so
// detector heuristics can search for framework names without parsing every
// manifest format.
func pythonManifestText(dir string) string {
	var parts []string
	for _, name := range []string{"pyproject.toml", "requirements.txt", "Pipfile", "poetry.lock"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			parts = append(parts, strings.ToLower(string(data)))
		}
	}
	return strings.Join(parts, "\n")
}

// pythonFileContains reports whether a Python source file contains a marker.
// Missing or unreadable files are treated as no match.
func pythonFileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

func hasPythonManifest(dir string) bool {
	for _, name := range []string{"pyproject.toml", "requirements.txt", "Pipfile", "poetry.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
