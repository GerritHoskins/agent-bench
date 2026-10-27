package bench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRejectsUnsupportedAndUnsafeInputs(t *testing.T) {
	cases := map[string]func(*Manifest){
		"live adapter":       func(m *Manifest) { m.Configurations[0].Adapter = "codex" },
		"unknown behavior":   func(m *Manifest) { m.Configurations[0].Behavior = "execute" },
		"duplicate ID":       func(m *Manifest) { m.Configurations = append(m.Configurations, m.Configurations[0]) },
		"unsafe ID":          func(m *Manifest) { m.Configurations[0].ID = "../escape" },
		"zero repetitions":   func(m *Manifest) { m.Policy.Repetitions = 0 },
		"unbounded timeout":  func(m *Manifest) { m.Policy.TimeoutMS = 10001 },
		"unbounded output":   func(m *Manifest) { m.Policy.MaxOutputBytes = 2 << 20 },
		"unbounded retries":  func(m *Manifest) { m.Policy.MaxInfrastructureRetries = 4 },
		"accepted budget":    func(m *Manifest) { m.Policy.BudgetStatus = "approved" },
		"unsafe scope":       func(m *Manifest) { m.Task.AllowedPaths = []string{"../escape"} },
		"arbitrary repo":     func(m *Manifest) { m.Task.Baseline["build.sh"] = "exit 0" },
		"missing tests":      func(m *Manifest) { delete(m.Task.Baseline, "tests.json") },
		"empty evaluator":    func(m *Manifest) { m.Task.Acceptance = nil },
		"duplicate tests":    func(m *Manifest) { m.Task.Acceptance = append(m.Task.Acceptance, m.Task.Acceptance[0]) },
		"unsupported schema": func(m *Manifest) { m.SchemaVersion = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixture(t, "success")
			mutate(&m)
			root := filepath.Join(t.TempDir(), "not-created")
			if _, err := CreateBundle(context.Background(), m, root); err == nil {
				t.Fatal("invalid input accepted")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("invalid manifest mutated filesystem")
			}
		})
	}
	for _, data := range []string{`{"schema_version":1,"command":"run"}`, `{} {}`, `[]`} {
		var m Manifest
		if err := decodeStrict([]byte(data), &m); err == nil {
			t.Fatalf("invalid JSON accepted: %s", data)
		}
	}
}

func TestPatchScopeAndBaselineMatching(t *testing.T) {
	before := map[string]string{"calculator.json": "original"}
	after := "modified"
	wrong := "wrong"
	for _, p := range []string{"../outside", "/absolute", `C:\outside`, ".git/config", "safe/../../escape", "tests.json"} {
		if _, err := applyPatch(before, Patch{SchemaVersion: 1, Changes: []Change{{Path: p, After: &after}}}, []string{"calculator.json"}); err == nil {
			t.Fatalf("unsafe patch accepted: %s", p)
		}
	}
	if _, err := applyPatch(before, Patch{SchemaVersion: 1, Changes: []Change{{Path: "calculator.json", Before: &wrong, After: &after}}}, []string{"calculator.json"}); err == nil {
		t.Fatal("mismatched baseline accepted")
	}
	original := "original"
	change := Change{Path: "calculator.json", Before: &original, After: &after}
	if _, err := applyPatch(before, Patch{SchemaVersion: 1, Changes: []Change{change, change}}, []string{"calculator.json"}); err == nil {
		t.Fatal("duplicate patch path accepted")
	}
	files, err := applyPatch(before, Patch{SchemaVersion: 1, Changes: []Change{change}}, []string{"calculator.json"})
	if err != nil || files["calculator.json"] != after || before["calculator.json"] != "original" {
		t.Fatal("patch corrupted baseline")
	}
}

func TestSnapshotCannotReuseExistingWorkspace(t *testing.T) {
	s, release, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.snapshot("attempts/fresh/workspace", map[string]string{"calculator.json": "dirty"}); err != nil {
		t.Fatal(err)
	}
	if err := s.snapshot("attempts/fresh/workspace", map[string]string{"calculator.json": "new"}); err == nil {
		t.Fatal("dirty workspace reused")
	}
	files, err := s.files("attempts/fresh/workspace", 128)
	if err != nil || files["calculator.json"] != "dirty" {
		t.Fatal("existing evidence overwritten")
	}
}

func TestCandidateOutputBound(t *testing.T) {
	s, release, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.snapshot("workspace", map[string]string{"calculator.json": strings.Repeat("x", 129)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.files("workspace", 128); err == nil {
		t.Fatal("output cap not enforced")
	}
}

func TestSymlinksAndTraversalAreRejected(t *testing.T) {
	root := t.TempDir()
	s, release, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, p := range []string{"../escape", "/escape", `C:\escape`, "dir/../escape", ".hidden/file"} {
		if err := s.write(p, []byte("bad")); err == nil {
			t.Fatalf("unsafe write accepted: %s", p)
		}
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.json")); err != nil {
		t.Skipf("symlink creation not permitted on this host: %v", err)
	}
	if err := s.write("link.json", []byte("changed")); err == nil {
		t.Fatal("symlink followed")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "unchanged" {
		t.Fatal("outside file modified")
	}
}

func TestPersistedArtifactsAndScheduleAreValidated(t *testing.T) {
	root := t.TempDir()
	result, err := CreateBundle(context.Background(), fixture(t, "success"), root)
	if err != nil {
		t.Fatal(err)
	}
	a := result.Attempts[0]
	a.Artifacts["bad"] = "https://example.com"
	data, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(attemptFile(Trial{ID: a.TrialID}, 1))), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportBundle(root); err == nil {
		t.Fatal("external artifact URL accepted")
	}
	root2 := t.TempDir()
	if _, err := CreateBundle(context.Background(), fixture(t, "success"), root2); err != nil {
		t.Fatal(err)
	}
	var run Run
	data, _ = os.ReadFile(filepath.Join(root2, "run.json"))
	_ = json.Unmarshal(data, &run)
	run.Schedule[0].ID = "arbitrary"
	data, _ = json.Marshal(run)
	if err := os.WriteFile(filepath.Join(root2, "run.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeBundle(context.Background(), root2); err == nil {
		t.Fatal("mutated schedule accepted")
	}
}

func TestLockReleasedAfterProcessExit(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockCrashHelper$")
	cmd.Env = append(os.Environ(), "AGENTBENCH_LOCK_HELPER_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lock helper: %v %s", err, output)
	}
	_, release, err := openStore(root)
	if err != nil {
		t.Fatalf("process exit left the bundle locked: %v", err)
	}
	release()
}

func TestLockCrashHelper(t *testing.T) {
	root := os.Getenv("AGENTBENCH_LOCK_HELPER_ROOT")
	if root == "" {
		return
	}
	_, _, err := openStore(root)
	if err != nil {
		os.Exit(2)
	}
	// Intentionally bypass deferred cleanup, as a crashed process would.
	os.Exit(0)
}
