package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agentbench/examples"
	"example.com/agentbench/internal/bench"
)

func invoke(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := command(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCLIEndToEndRunResumeReport(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "task.json")
	bundle := filepath.Join(root, "bundle")
	var m bench.Manifest
	if err := json.Unmarshal(examples.Demo, &m); err != nil {
		t.Fatal(err)
	}
	m.Policy.Repetitions = 1
	m.Configurations = []bench.Configuration{{ID: "interrupted", Adapter: "fake", Behavior: "interrupted"}}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := invoke("run", "--manifest", manifest, "--out", bundle)
	if code != 130 || !strings.Contains(out, "Report:") || !strings.Contains(errOut, "resume") {
		t.Fatalf("interruption: %d %s %s", code, out, errOut)
	}
	code, out, errOut = invoke("resume", "--bundle", bundle)
	if code != 0 || !strings.Contains(out, "1/1 passed") || !strings.Contains(out, "1 infrastructure attempts") {
		t.Fatalf("resume: %d %s %s", code, out, errOut)
	}
	code, out, errOut = invoke("report", "--bundle", bundle)
	if code != 0 || !strings.Contains(out, "resolved=true") {
		t.Fatalf("report: %d %s %s", code, out, errOut)
	}
	if code, _, _ := invoke("run", "--manifest", manifest, "--out", bundle); code != 1 {
		t.Fatal("overwriting existing bundle accepted")
	}
}

func TestCLIDemoExposesUnresolvedSamples(t *testing.T) {
	code, out, errOut := invoke("demo", "--out", filepath.Join(t.TempDir(), "demo"))
	if code != 1 || !strings.Contains(out, "fixed: 3/3 passed") || !strings.Contains(out, "bad-output: 0/0 passed; 3 unresolved; 6 infrastructure attempts") || !strings.Contains(errOut, "no valid sample") {
		t.Fatalf("demo: %d %s %s", code, out, errOut)
	}
}

func TestCLIUsageAndInvalidInput(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"demo", "--help"}} {
		if code, _, _ := invoke(args...); code != 0 {
			t.Fatalf("help failed: %v", args)
		}
	}
	for _, args := range [][]string{{"unknown"}, {"demo"}, {"run", "--out", "unused"}, {"resume"}, {"report"}, {"demo", "extra"}, {"demo", "--unknown"}} {
		if code, _, _ := invoke(args...); code != 2 {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if code, _, _ := invoke("run", "--out", filepath.Join(t.TempDir(), "bundle"), "--manifest", filepath.Join(t.TempDir(), "missing.json")); code != 1 {
		t.Fatal("missing manifest accepted")
	}
}
