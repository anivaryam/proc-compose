package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJUnit_CountsCasesNotNestedSuiteTotals(t *testing.T) {
	data := `<testsuites tests="4" failures="1" errors="1" skipped="1"><testsuite tests="4"><testsuite tests="4"><testcase name="ok"/><testcase name="fail" classname="suite"><failure message="expected &lt; got"/></testcase><testcase name="error"><error>stack trace</error></testcase><testcase name="skip"><skipped/></testcase></testsuite></testsuite></testsuites>`
	s, err := parseJUnit([]byte(data))
	if err != nil || s.Status != "parsed" || s.Tests != 4 || s.Failures != 1 || s.Errors != 1 || s.Skipped != 1 || len(s.Examples) != 2 || s.Examples[0].Message != "expected < got" || s.Examples[1].Message != "stack trace" {
		t.Fatalf("JUnit summary: %+v %v", s, err)
	}
}

func TestJUnit_NodeReporterRootCases(t *testing.T) {
	// Node 22's built-in JUnit reporter places top-level tests directly in
	// testsuites, alongside testsuite elements for describe() groups.
	data := `<?xml version="1.0" encoding="utf-8"?>
<testsuites>
  <testcase name="top-level pass" time="0.001133" classname="test"/>
  <testcase name="top-level failure" time="0.001985" classname="test"><failure type="testCodeFailure" message="expected equality">assertion details</failure></testcase>
  <testcase name="top-level skip" classname="test"><skipped/></testcase>
  <testsuite name="group" tests="1" errors="1"><testcase name="group error"><error>stack trace</error></testcase></testsuite>
  <!-- tests 4 -->
</testsuites>`
	s, err := parseJUnit([]byte(data))
	if err != nil || s.Status != "parsed" || s.Tests != 4 || s.Failures != 1 || s.Errors != 1 || s.Skipped != 1 || len(s.Examples) != 2 || s.Examples[0].Name != "top-level failure" || s.Examples[0].Message != "expected equality" {
		t.Fatalf("Node JUnit summary: %+v %v", s, err)
	}
}

func TestJUnit_RejectsMalformedOrUnsupportedReports(t *testing.T) {
	for _, data := range []string{
		``, `<unrelated/>`, `<testsuite>`, `<testsuite/><testsuite/>`,
		`<!DOCTYPE testsuite><testsuite/>`, `<testsuite>&external;</testsuite>`,
		`<testsuite tests="1"/>`, `<testsuite failures="1"><testcase name="claimed-pass"/></testsuite>`,
		`<testsuite tests="-1"/>`, `<testsuite><failure/></testsuite>`,
		`<testsuite><testcase name="a" name="b"/></testsuite>`,
		`<testsuite><testcase><testcase/></testcase></testsuite>`,
		`<testsuites><testcase><testcase/></testcase></testsuites>`,
		`<testsuites><properties><testcase/></properties></testsuites>`,
		strings.Repeat(`<testsuite>`, 65) + strings.Repeat(`</testsuite>`, 65),
	} {
		t.Run(fmt.Sprintf("case-%d", len(data)), func(t *testing.T) {
			if s, err := parseJUnit([]byte(data)); err == nil {
				t.Fatalf("accepted %q: %+v", data, s)
			}
		})
	}
	if s, err := parseJUnit([]byte(`<testsuite tests="0"/>`)); err != nil || s.Status != "empty" {
		t.Fatalf("empty suite: %+v %v", s, err)
	}
}

func TestArtifacts_BoundedIndexAndFailureExamplesPreserveRaw(t *testing.T) {
	dir := t.TempDir()
	report := `<testsuite>` + strings.Repeat(`<testcase name="bad"><failure>`+strings.Repeat("bad", 300)+`</failure></testcase>`, 20) + `</testsuite>`
	path := filepath.Join(dir, JUnitFile)
	if err := os.WriteFile(path, []byte(report), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range maxArtifactEntries + 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d-output.txt", i)), []byte("kept"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := collectArtifacts(dir)
	data, err := json.Marshal(a)
	if err != nil || len(data) > maxArtifactMetadata || !a.Truncated || !a.JUnit.Truncated || a.JUnit.Tests != 20 || a.JUnit.Failures != 20 || len(a.JUnit.Examples) > maxTestFailures {
		t.Fatalf("unbounded metadata: %d bytes %+v %v", len(data), a, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != report {
		t.Fatal("raw report changed")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxJUnitBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if s := readJUnit(path); s.Status != "too_large" {
		t.Fatalf("oversize report: %+v", s)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if s := readJUnit(path); s.Status != "missing" {
		t.Fatalf("missing report: %+v", s)
	}
	if err := os.WriteFile(path, []byte(`<testsuite><testcase/><broken>`), 0600); err != nil {
		t.Fatal(err)
	}
	if s := readJUnit(path); s.Status != "invalid" || s.Tests != 0 {
		t.Fatalf("partial malformed counts: %+v", s)
	}
}
