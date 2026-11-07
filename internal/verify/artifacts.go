package verify

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	MaxJUnitBytes       = 1 << 20
	maxArtifactEntries  = 64
	maxArtifactScan     = 256
	maxArtifactMetadata = 8 << 10
	maxTestFailures     = 5
	JUnitFile           = "junit.xml"
)

// Artifacts describes files written by the check, not independently attested
// evidence. Limits bound indexing and summaries, not the check's disk usage.
type Artifacts struct {
	Directory string         `json:"directory"`
	Status    string         `json:"status"`
	Files     []ArtifactFile `json:"files"`
	Skipped   int            `json:"skipped"`
	Truncated bool           `json:"truncated"`
	JUnit     JUnitSummary   `json:"junit"`
}

type ArtifactFile struct {
	Name  string `json:"name"` // relative to Directory
	Bytes int64  `json:"bytes"`
}

type JUnitSummary struct {
	Path      string        `json:"path"` // relative to artifact directory; raw report is preserved
	Status    string        `json:"status"`
	Tests     int           `json:"tests"`
	Failures  int           `json:"failures"`
	Errors    int           `json:"errors"`
	Skipped   int           `json:"skipped"`
	Examples  []TestFailure `json:"failure_examples"`
	Truncated bool          `json:"truncated"`
	Message   string        `json:"message,omitempty"`
}

type TestFailure struct {
	Name    string `json:"name"`
	Class   string `json:"class"`
	Message string `json:"message"`
}

func artifactDirectory(evidenceDir, attemptID string) string {
	return filepath.Join(evidenceDir, "artifacts", attemptID)
}

func collectArtifacts(dir string) *Artifacts {
	a := &Artifacts{Directory: dir, Status: "collected", Files: []ArtifactFile{}, JUnit: JUnitSummary{Path: JUnitFile, Status: "missing", Examples: []TestFailure{}}}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		a.Status, a.JUnit.Status = "unavailable", "unverified"
		return a
	}
	// Only reached after owned writers have terminated. Refuse symlinks and
	// special files; never follow them or open application-created devices.
	scanned := 0
	var walk func(string, int)
	walk = func(relative string, depth int) {
		if depth > 16 || scanned >= maxArtifactScan || len(a.Files) >= maxArtifactEntries {
			a.Truncated = true
			return
		}
		f, err := os.Open(filepath.Join(dir, relative))
		if err != nil {
			a.Skipped++
			return
		}
		defer f.Close()
		for {
			entries, err := f.Readdir(32)
			for _, entry := range entries {
				scanned++
				if scanned > maxArtifactScan || len(a.Files) >= maxArtifactEntries {
					a.Truncated = true
					return
				}
				name := filepath.Join(relative, entry.Name())
				switch {
				case len(name) > 512:
					a.Skipped++
				case entry.IsDir():
					walk(name, depth+1)
				case entry.Mode().IsRegular():
					a.Files = append(a.Files, ArtifactFile{Name: filepath.ToSlash(name), Bytes: entry.Size()})
				default:
					a.Skipped++
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					a.Skipped++
				}
				return
			}
		}
	}
	walk("", 0)
	sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Name < a.Files[j].Name })
	a.JUnit = readJUnit(filepath.Join(dir, JUnitFile))
	// Escaped file names/messages can be much larger than their raw strings.
	// Trim optional examples/index entries, preserving counts and the raw files.
	for {
		data, _ := json.Marshal(a)
		if len(data) <= maxArtifactMetadata {
			break
		}
		a.Truncated = true
		if len(a.Files) > 0 {
			a.Files = a.Files[:len(a.Files)-1]
		} else if len(a.JUnit.Examples) > 0 {
			a.JUnit.Examples = a.JUnit.Examples[:len(a.JUnit.Examples)-1]
			a.JUnit.Truncated = true
		} else {
			break // an extreme directory path remains subject to Encode's cap
		}
	}
	return a
}

func readJUnit(path string) JUnitSummary {
	s := JUnitSummary{Path: JUnitFile, Status: "invalid", Examples: []TestFailure{}}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		s.Status = "missing"
		return s
	}
	if err != nil || !info.Mode().IsRegular() {
		s.Message = "report is not a readable regular file"
		return s
	}
	if info.Size() > MaxJUnitBytes {
		s.Status = "too_large"
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		s.Message = "report could not be opened"
		return s
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxJUnitBytes+1))
	if err != nil {
		s.Message = "report could not be read"
		return s
	}
	if len(data) > MaxJUnitBytes {
		s.Status = "too_large"
		return s
	}
	s, err = parseJUnit(data)
	if err != nil {
		// Partial counts from malformed XML are not a test summary.
		return JUnitSummary{Path: JUnitFile, Status: "invalid", Examples: []TestFailure{}, Message: truncateUTF8(err.Error(), 256)}
	}
	return s
}

// parseJUnit counts testcase elements, never aggregate suite counters (which
// are often repeated by nested suites). Only bounded failure examples are kept.
func parseJUnit(data []byte) (JUnitSummary, error) {
	s := JUnitSummary{Path: JUnitFile, Status: "parsed", Examples: []TestFailure{}}
	d := xml.NewDecoder(bytes.NewReader(data))
	var stack []string
	var current TestFailure
	caseDepth, detailDepth := 0, 0
	failed, errored, skipped, rootSeen, declaredTests, declaredFailure := false, false, false, false, false, false
	for {
		token, err := d.Token()
		if errors.Is(err, io.EOF) {
			if !rootSeen || len(stack) != 0 || (s.Tests == 0 && declaredTests) {
				return s, errors.New("report must contain a complete testsuite/testsuites tree with testcase details")
			}
			if declaredFailure && s.Failures == 0 && s.Errors == 0 {
				return s, errors.New("suite declares failures without testcase failure details")
			}
			if s.Tests == 0 {
				s.Status = "empty"
			}
			return s, nil
		}
		if err != nil {
			return s, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if len(stack) >= 64 {
				return s, errors.New("report nesting exceeds 64 levels")
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			} else {
				if rootSeen || (name != "testsuite" && name != "testsuites") {
					return s, errors.New("report must have one testsuite or testsuites root")
				}
				rootSeen = true
			}
			attrs := map[string]string{}
			for _, attr := range t.Attr {
				if _, exists := attrs[attr.Name.Local]; exists {
					return s, errors.New("report repeats an XML attribute")
				}
				attrs[attr.Name.Local] = attr.Value
			}
			stack = append(stack, name)
			switch name {
			case "testsuite", "testsuites":
				if parent != "" && parent != "testsuite" && parent != "testsuites" {
					return s, errors.New("suite is outside the suite tree")
				}
				for _, key := range []string{"tests", "failures", "errors", "skipped"} {
					if value, ok := attrs[key]; ok {
						n, err := strconv.ParseInt(value, 10, 64)
						if err != nil || n < 0 {
							return s, fmt.Errorf("invalid suite %s count", key)
						}
						declaredTests = declaredTests || n > 0
						declaredFailure = declaredFailure || (n > 0 && (key == "failures" || key == "errors"))
					}
				}
			case "testcase":
				// Node's built-in JUnit reporter emits top-level tests directly
				// under testsuites; describe() groups still use testsuite.
				if caseDepth != 0 || (parent != "testsuite" && parent != "testsuites") {
					return s, errors.New("testcase is outside a testsuite/testsuites container")
				}
				caseDepth = len(stack)
				current = TestFailure{Name: truncateUTF8(attrs["name"], 256), Class: truncateUTF8(attrs["classname"], 256)}
				s.Truncated = s.Truncated || len(attrs["name"]) > 256 || len(attrs["classname"]) > 256
				failed, errored, skipped = false, false, false
			case "failure", "error", "skipped":
				if caseDepth == 0 || parent != "testcase" {
					return s, errors.New("test outcome is outside a testcase")
				}
				failed, errored, skipped = failed || name == "failure", errored || name == "error", skipped || name == "skipped"
				if name != "skipped" && current.Message == "" {
					current.Message = truncateUTF8(attrs["message"], 512)
					s.Truncated = s.Truncated || len(attrs["message"]) > 512
					if current.Message == "" {
						detailDepth = len(stack)
					}
				}
			}
		case xml.EndElement:
			if len(stack) == caseDepth && t.Name.Local == "testcase" {
				s.Tests++
				if failed {
					s.Failures++
				}
				if errored {
					s.Errors++
				}
				if skipped && !failed && !errored {
					s.Skipped++
				}
				if failed || errored {
					if len(s.Examples) < maxTestFailures {
						s.Examples = append(s.Examples, current)
					} else {
						s.Truncated = true
					}
				}
				caseDepth = 0
			}
			if len(stack) == detailDepth {
				detailDepth = 0
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(t)) != "" {
				return s, errors.New("text outside the report root")
			}
			if detailDepth > 0 {
				s.Truncated = s.Truncated || len(current.Message)+len(t) > 512
				if len(current.Message) < 512 {
					current.Message = truncateUTF8(current.Message+string(t), 512)
				}
			}
		case xml.Directive:
			return s, errors.New("XML directives are not supported")
		}
	}
}
