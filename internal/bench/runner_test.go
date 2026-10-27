package bench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"example.com/agentbench/examples"
)

func fixture(t *testing.T, behavior string) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(examples.Demo, &m); err != nil {
		t.Fatal(err)
	}
	if behavior != "" {
		m.Configurations = []Configuration{{ID: "subject", Adapter: "fake", Behavior: behavior}}
		m.Policy.Repetitions = 1
	}
	m.Policy.TimeoutMS = 5
	return m
}

func TestFakeOutcomesAndAccounting(t *testing.T) {
	cases := []struct {
		behavior string
		failure  FailureClass
		counted  bool
		attempts int
	}{
		{"success", "", true, 1}, {"alternate_success", "", true, 1},
		{"failure", TaskFailure, true, 1}, {"timeout", Timeout, true, 1},
		{"malformed", RunnerFailure, false, 2}, {"tamper", TaskFailure, true, 1},
		{"out_of_scope", InvalidPatch, true, 1}, {"runner_failure", RunnerFailure, false, 2},
		{"auth_failure", AuthFailure, false, 2}, {"provider_outage", ProviderOutage, false, 2},
		{"evaluator_failure", EvaluatorFailure, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.behavior, func(t *testing.T) {
			root := t.TempDir()
			result, err := CreateBundle(context.Background(), fixture(t, tc.behavior), root)
			if tc.counted && err != nil {
				t.Fatal(err)
			}
			if !tc.counted && !errors.Is(err, ErrUnresolved) {
				t.Fatalf("expected unresolved, got %v", err)
			}
			if len(result.Attempts) != tc.attempts {
				t.Fatalf("attempts=%d want %d", len(result.Attempts), tc.attempts)
			}
			for _, a := range result.Attempts {
				if a.Failure != tc.failure || a.Counted != tc.counted {
					t.Fatalf("wrong outcome: %+v", a)
				}
				if a.Cost.Provenance != Unavailable || a.Cost.USD != nil {
					t.Fatalf("unknown cost must not be zero: %+v", a.Cost)
				}
			}
			s := result.Summaries[0]
			if tc.counted && (s.Samples != 1 || s.Unresolved != 0) {
				t.Fatalf("wrong samples: %+v", s)
			}
			if !tc.counted && (s.Samples != 0 || s.InfrastructureAttempts != tc.attempts || s.Unresolved != 1) {
				t.Fatalf("infrastructure counted as samples: %+v", s)
			}
			if _, err := os.Stat(filepath.Join(root, "report.html")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDemoTrustedEvaluationAndDifferentPatches(t *testing.T) {
	root := t.TempDir()
	result, err := CreateBundle(context.Background(), fixture(t, ""), root)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("demo malformed config must be unresolved: %v", err)
	}
	if len(result.Attempts) != 21 {
		t.Fatalf("got %d attempts", len(result.Attempts))
	}
	var fixed, alternate string
	for _, a := range result.Attempts {
		if a.ConfigurationID == "test-tamper" {
			if a.Failure != TaskFailure || len(a.Assertions) != 3 || allPassed(a.Assertions) {
				t.Fatal("tampering influenced trusted evaluation")
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(attemptDir(Trial{ID: a.TrialID}, a.Number)), "workspace", "tests.json"))
			if err != nil || string(data) != "[]\n" {
				t.Fatal("tampering scenario did not actually alter repository tests")
			}
		}
		if a.Repetition == 1 && (a.ConfigurationID == "fixed" || a.ConfigurationID == "fixed-alternate") {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.Artifacts["patch"])))
			if err != nil {
				t.Fatal(err)
			}
			if a.ConfigurationID == "fixed" {
				fixed = string(b)
			} else {
				alternate = string(b)
			}
		}
	}
	if fixed == "" || alternate == "" || fixed == alternate {
		t.Fatal("isolated successful configurations must retain distinct patches")
	}
	for _, s := range result.Summaries {
		if s.ConfigurationID == "bad-output" {
			if s.Samples != 0 || s.Unresolved != 3 || s.InfrastructureAttempts != 6 {
				t.Fatalf("wrong malformed accounting: %+v", s)
			}
		} else if s.Samples != 3 {
			t.Fatalf("missing repetitions: %+v", s)
		}
	}
}

func TestInterruptedRecoveryNeverReusesDirtyWorkspace(t *testing.T) {
	root := t.TempDir()
	first, err := CreateBundle(context.Background(), fixture(t, "interrupted"), root)
	if !errors.Is(err, ErrInterrupted) || len(first.Attempts) != 1 || first.Attempts[0].State != Running {
		t.Fatalf("expected running checkpoint: %v %+v", err, first)
	}
	a := first.Attempts[0]
	old := filepath.Join(root, filepath.FromSlash(attemptDir(Trial{ID: a.TrialID}, 1)), "workspace")
	if err := os.WriteFile(filepath.Join(old, "stale-marker.txt"), []byte("must not be copied"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeBundle(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Complete || len(resumed.Attempts) != 2 || resumed.Attempts[0].Failure != RunnerFailure || resumed.Attempts[0].Counted || resumed.Attempts[1].State != Done {
		t.Fatalf("wrong recovery: %+v", resumed)
	}
	oldData, err := os.ReadFile(filepath.Join(old, "calculator.json"))
	if err != nil || !strings.Contains(string(oldData), "999") {
		t.Fatal("dirty evidence was overwritten")
	}
	fresh := filepath.Join(root, filepath.FromSlash(attemptDir(Trial{ID: a.TrialID}, 2)), "workspace", "stale-marker.txt")
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("dirty workspace reused")
	}
	again, err := ResumeBundle(context.Background(), root)
	if err != nil || !reflect.DeepEqual(resumed.Attempts, again.Attempts) {
		t.Fatal("resume re-executed a completed trial")
	}
}

func TestRecoveryAtEveryCheckpoint(t *testing.T) {
	for _, state := range []State{Pending, Preparing, BaselineValidated, Running, Collecting, Evaluating, Done, Failed} {
		t.Run(string(state), func(t *testing.T) {
			root := t.TempDir()
			behavior := "success"
			if state == Failed {
				behavior = "failure"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := CreateBundle(ctx, fixture(t, behavior), root); !errors.Is(err, ErrInterrupted) {
				t.Fatal(err)
			}
			s, release, err := openStore(root)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.loadRun()
			if err != nil {
				release()
				t.Fatal(err)
			}
			crash := errors.New("simulated crash after durable checkpoint")
			_, err = runAndReport(context.Background(), s, run, func(a Attempt) error {
				if a.State == state {
					return crash
				}
				return nil
			})
			release()
			if !errors.Is(err, crash) {
				t.Fatalf("crash hook did not run: %v", err)
			}
			result, err := ResumeBundle(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if state == Done || state == Failed {
				want = 1
			}
			if !result.Complete || len(result.Attempts) != want || result.Summaries[0].Samples != 1 {
				t.Fatalf("wrong checkpoint recovery: %+v", result)
			}
		})
	}
}

func TestBaselineFailureNeverRunsAdapter(t *testing.T) {
	m := fixture(t, "success")
	m.Task.Baseline["tests.json"] = `[{"name":"existing","a":1,"b":1,"want":2}]`
	result, err := CreateBundle(context.Background(), m, t.TempDir())
	if !errors.Is(err, ErrUnresolved) || len(result.Attempts) != 1 {
		t.Fatalf("baseline failure retried: %v", err)
	}
	a := result.Attempts[0]
	if a.Failure != BaselineFailure || a.Counted {
		t.Fatalf("wrong baseline failure: %+v", a)
	}
	for _, h := range a.History {
		if h.State == Running {
			t.Fatal("agent ran on a pre-existing failure")
		}
	}
}

func TestFrozenInputChangesAreRejected(t *testing.T) {
	for _, name := range []string{"baseline.json", "evaluator.json"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := CreateBundle(context.Background(), fixture(t, "success"), root); err != nil {
				t.Fatal(err)
			}
			value := []byte("[]")
			if name == "baseline.json" {
				value = []byte("{}")
			}
			if err := os.WriteFile(filepath.Join(root, "inputs", name), value, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ResumeBundle(context.Background(), root); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
				t.Fatalf("modified frozen input accepted: %v", err)
			}
		})
	}
}

func TestCleanupFailureStopsBeforeNextTrial(t *testing.T) {
	root := t.TempDir()
	m := fixture(t, "success")
	m.Policy.Repetitions = 2
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = CreateBundle(ctx, m, root)
	s, release, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.loadRun()
	if err != nil {
		release()
		t.Fatal(err)
	}
	blocked := false
	_, err = runAndReport(context.Background(), s, run, func(a Attempt) error {
		if a.State == Evaluating && !blocked {
			blocked = true
			p, _ := s.path(attemptDir(run.Schedule[0], 1) + "/cleanup.json")
			return os.Mkdir(p, 0700)
		}
		return nil
	})
	release()
	if err == nil {
		t.Fatal("failed cleanup did not stop execution")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(attemptFile(run.Schedule[1], 1)))); !os.IsNotExist(err) {
		t.Fatal("next trial ran after cleanup failed")
	}
	if _, err := ResumeBundle(context.Background(), root); err == nil {
		t.Fatal("resume ignored failed cleanup")
	}
}

func TestCompletedCleanupMustStillBePresent(t *testing.T) {
	root := t.TempDir()
	result, err := CreateBundle(context.Background(), fixture(t, "success"), root)
	if err != nil {
		t.Fatal(err)
	}
	a := result.Attempts[0]
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(a.Artifacts["cleanup"]))); err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeBundle(context.Background(), root); err == nil || !strings.Contains(err.Error(), "cleanup not confirmed") {
		t.Fatalf("missing cleanup accepted: %v", err)
	}
	if _, err := ReportBundle(root); err == nil {
		t.Fatal("report accepted missing cleanup evidence")
	}
}

func TestReportIsOfflineEscapedAndAllLinksResolve(t *testing.T) {
	root := t.TempDir()
	m := fixture(t, "tamper")
	m.Task.Prompt = `<script>alert("x")</script>`
	result, err := CreateBundle(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	if strings.Contains(html, "<script>") || strings.Contains(html, "http://") || strings.Contains(html, "https://") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("report not escaped or not offline")
	}
	for _, match := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if !safePath(match[1]) {
			t.Fatalf("unsafe link %s", match[1])
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(match[1]))); err != nil {
			t.Fatalf("broken evidence link %s: %v", match[1], err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "results.json"), []byte("corrupt derived file"), 0600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := ReportBundle(root)
	if err != nil || !reflect.DeepEqual(result, rebuilt) {
		t.Fatalf("report did not rebuild from checkpoints: %v", err)
	}
}

func TestExclusiveBundleLockAndNoOverwrite(t *testing.T) {
	root := t.TempDir()
	_, release, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, release2, err := openStore(root); err == nil {
		release2()
		t.Fatal("concurrent writer accepted")
	}
	release()
	if _, err := CreateBundle(context.Background(), fixture(t, "success"), root); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBundle(context.Background(), fixture(t, "success"), root); err == nil {
		t.Fatal("existing evidence overwritten")
	}
}

func TestRandomInterleavingIsSeededAndBalanced(t *testing.T) {
	m := fixture(t, "")
	first := schedule(m)
	if !reflect.DeepEqual(first, schedule(m)) {
		t.Fatal("seed not reproducible")
	}
	m.Policy.Seed++
	if reflect.DeepEqual(first, schedule(m)) {
		t.Fatal("seed not used")
	}
	n := len(m.Configurations)
	for i := 0; i < len(first); i += n {
		seen := map[string]bool{}
		for _, trial := range first[i : i+n] {
			if seen[trial.ConfigurationID] || trial.Repetition != i/n+1 {
				t.Fatal("unbalanced repetition")
			}
			seen[trial.ConfigurationID] = true
		}
	}
}
