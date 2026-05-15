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
	Scripts      map[string]string `json:"scripts"`
	Dependencies map[string]string `json:"dependencies"`
	DevDeps      map[string]string `json:"devDependencies"`
	Workspaces   json.RawMessage   `json:"workspaces"`
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
		if isAggregateNodePackage(pkg) {
			return nil
		}

		script := ""
		if pkg.Scripts != nil {
			if _, ok := pkg.Scripts["dev"]; ok {
				script = "npm run dev"
			} else if _, ok := pkg.Scripts["start"]; ok {
				script = "npm run start"
			}
		}
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

func detectNodePort(dir string, pkg *packageJSON) int {
	if port := detectEnvPort(dir); port > 0 {
		return port
	}

	hasVite := false
	hasNext := false
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
		}
	}

	if pkg.Dependencies != nil {
		if _, ok := pkg.Dependencies["vite"]; ok {
			hasVite = true
		}
		if _, ok := pkg.Dependencies["next"]; ok {
			hasNext = true
		}
	}
	if pkg.DevDeps != nil {
		if _, ok := pkg.DevDeps["vite"]; ok {
			hasVite = true
		}
		if _, ok := pkg.DevDeps["next"]; ok {
			hasNext = true
		}
	}

	if hasVite {
		return 5173
	}
	if hasNext {
		return 3000
	}
	return 0
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

func detectEnvPort(dir string) int {
	for _, filename := range []string{".env.example", ".env"} {
		envPath := filepath.Join(dir, filename)
		content, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}

		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "PORT=") {
				portStr := strings.TrimPrefix(line, "PORT=")
				portStr = strings.Trim(portStr, `"'`)
				if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
					return p
				}
			}
		}
	}
	return 0
}

func scanGoServices(root string, report *Report) error {
	goModPath := filepath.Join(root, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		return nil
	}

	cmdDir := filepath.Join(root, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		addRootGoService(root, report)
		return nil
	}

	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !safeProcessNameRe.MatchString(entry.Name()) {
			continue
		}
		svcDir := filepath.Join(cmdDir, entry.Name())
		mainPath := filepath.Join(svcDir, "main.go")
		if !isGoMain(mainPath) {
			continue
		}

		report.Services = append(report.Services, Service{
			Name:       sanitizeProcessName(entry.Name()),
			Command:    "go run .",
			Dir:        "./cmd/" + entry.Name(),
			Port:       0,
			Kind:       "go",
			Confidence: ConfidenceMedium,
			Manifest:   "go.mod",
		})
		found = true
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

func detectPythonCommand(dir string) (string, string, int, Confidence) {
	port := detectEnvPort(dir)
	if _, err := os.Stat(filepath.Join(dir, "manage.py")); err == nil {
		if port == 0 {
			port = 8000
		}
		return fmt.Sprintf("python manage.py runserver 0.0.0.0:%d", port), "manage.py", port, ConfidenceHigh
	}
	if _, err := os.Stat(filepath.Join(dir, "app.py")); err == nil && hasPythonManifest(dir) {
		return "python app.py", "app.py", port, ConfidenceMedium
	}
	if _, err := os.Stat(filepath.Join(dir, "main.py")); err == nil && hasPythonManifest(dir) {
		return "python main.py", "main.py", port, ConfidenceMedium
	}
	return "", "", 0, ConfidenceLow
}

func hasPythonManifest(dir string) bool {
	for _, name := range []string{"pyproject.toml", "requirements.txt", "Pipfile", "poetry.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
