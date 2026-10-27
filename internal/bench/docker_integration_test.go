package bench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerFixture(t *testing.T, behavior string) Manifest {
	t.Helper()
	if os.Getenv("AGENTBENCH_DOCKER_TEST") != "1" {
		t.Skip("opt in with AGENTBENCH_DOCKER_TEST=1 and a locally built fake image")
	}
	m := fixture(t, behavior)
	image := os.Getenv("AGENTBENCH_DOCKER_IMAGE")
	if image == "" {
		image = "agentbench-fake:local"
	}
	m.Execution = &Execution{Backend: "docker", Image: image}
	m.Policy.TimeoutMS = 2000
	return m
}

func assertNoRunContainers(t *testing.T, runID string) {
	t.Helper()
	out, err := management(installedDocker{}, "container", "ls", "--all", "--filter", "label=agentbench.run="+runID, "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(string(out.stdout)) != "" {
		t.Fatalf("run containers remain or absence unverified: %v %s", err, out.stdout)
	}
}

func TestDockerIntegrationIsolationAndOutcomes(t *testing.T) {
	m := dockerFixture(t, "success")
	m.Policy.MaxInfrastructureRetries = 0
	m.Configurations = []Configuration{}
	for _, b := range []string{"success", "isolation", "tamper", "output_limit", "process_tree", "process_tree_timeout"} {
		m.Configurations = append(m.Configurations, Configuration{ID: b, Adapter: "fake", Behavior: b})
	}
	t.Setenv("AGENTBENCH_HOST_SENTINEL", "must-never-enter-container")
	root := t.TempDir()
	hostFile := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(hostFile, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := CreateBundle(context.Background(), m, root)
	if !errors.Is(err, ErrUnresolved) || len(result.Attempts) != 6 {
		t.Fatalf("unexpected fixture result: %v %+v", err, result)
	}
	for _, a := range result.Attempts {
		if a.Docker == nil || !a.Docker.IsolationVerified || !a.Docker.Started || !a.Docker.Stopped || !a.Docker.Removed {
			t.Fatalf("unverified lifecycle: %+v", a)
		}
		switch a.ConfigurationID {
		case "success", "isolation", "process_tree":
			if a.State != Done {
				t.Fatalf("expected pass: %+v", a)
			}
		case "tamper":
			if a.Failure != TaskFailure || len(a.Assertions) != 3 {
				t.Fatal("container tampering affected trusted evaluator")
			}
		case "output_limit":
			if a.Failure != RunnerFailure || a.Counted {
				t.Fatal("output flood was not a bounded infrastructure failure")
			}
		case "process_tree_timeout":
			if a.Failure != Timeout || a.Docker.ChildPID < 2 || !strings.Contains(a.Docker.ProcessesBeforeCleanup, "--child") {
				t.Fatalf("no running descendant evidence: %+v", a.Docker)
			}
		}
		if a.ConfigurationID == "isolation" {
			if len(a.Docker.Probes) != 8 {
				t.Fatalf("missing probes: %+v", a.Docker.Probes)
			}
			for name, passed := range a.Docker.Probes {
				if !passed {
					t.Fatalf("isolation failed: %s", name)
				}
			}
		}
		if a.ConfigurationID == "process_tree" && a.Docker.ChildPID < 2 {
			t.Fatal("background-child fixture did not spawn")
		}
		for _, name := range []string{"docker-stdout.jsonl", "docker-stderr.txt"} {
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(a.Artifacts[name])))
			if err != nil || info.Size() > int64(m.Policy.MaxOutputBytes) {
				t.Fatalf("raw output not bounded: %v", err)
			}
		}
	}
	data, _ := os.ReadFile(hostFile)
	if string(data) != "unchanged" {
		t.Fatal("container altered a host file")
	}
	assertNoRunContainers(t, result.RunID)
	if _, err := ReportBundle(root); err != nil {
		t.Fatal(err)
	}
}

func TestDockerIntegrationInterruptionAndResume(t *testing.T) {
	m := dockerFixture(t, "interrupted")
	root := t.TempDir()
	first, err := CreateBundle(context.Background(), m, root)
	if !errors.Is(err, ErrInterrupted) || len(first.Attempts) != 1 || !first.Attempts[0].Docker.Removed {
		t.Fatalf("interruption did not clean up: %v %+v", err, first)
	}
	result, err := ResumeBundle(context.Background(), root)
	if err != nil || !result.Complete || len(result.Attempts) != 2 || result.Attempts[0].Failure != RunnerFailure || result.Attempts[1].State != Done {
		t.Fatalf("Docker recovery failed: %v %+v", err, result)
	}
	if result.Attempts[0].Docker.Name == result.Attempts[1].Docker.Name {
		t.Fatal("retry reused container identity")
	}
	old, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(attemptDir(Trial{ID: first.Attempts[0].TrialID}, 1)), "workspace", "calculator.json"))
	if err != nil || !strings.Contains(string(old), "999") {
		t.Fatal("dirty container output was not preserved")
	}
	assertNoRunContainers(t, result.RunID)
}

// Wait for an actual descendant in this test's container before cancellation or
// a controller crash. A created-but-never-started container would not prove it.
func waitForDockerChild(t *testing.T, root string) (Run, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(root, "run.json"))
		var run Run
		if err == nil && json.Unmarshal(data, &run) == nil && run.ID != "" {
			listed, err := management(installedDocker{}, "container", "ls", "--filter", "label=agentbench.run="+run.ID, "--format", "{{.ID}}")
			if err == nil {
				for _, id := range strings.Fields(string(listed.stdout)) {
					top, err := management(installedDocker{}, "container", "top", id, "-eo", "pid,ppid,args")
					if err == nil && strings.Contains(string(top.stdout), "--child") {
						return run, id
					}
				}
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatal("timed out waiting for actual container child process")
	return Run{}, ""
}

func TestDockerIntegrationCancellationKillsDescendants(t *testing.T) {
	m := dockerFixture(t, "process_tree_timeout")
	m.Policy.TimeoutMS = 10000
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	drained := false
	t.Cleanup(func() {
		cancel()
		if !drained {
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("test controller cleanup did not finish")
			}
		}
	})
	go func() { result, err := CreateBundle(ctx, m, root); done <- outcome{result, err} }()
	run, _ := waitForDockerChild(t, root)
	cancel()
	select {
	case finished := <-done:
		drained = true
		if !errors.Is(finished.err, ErrInterrupted) || len(finished.result.Attempts) != 1 || !finished.result.Attempts[0].Docker.Removed {
			t.Fatalf("cancel failed to clean descendants: %v %+v", finished.err, finished.result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation cleanup did not finish")
	}
	assertNoRunContainers(t, run.ID)
}

func TestDockerIntegrationCrashRecoversOrphan(t *testing.T) {
	_ = dockerFixture(t, "success")
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDockerOrphanHelper$")
	cmd.Env = append(os.Environ(), "AGENTBENCH_DOCKER_ORPHAN_ROOT="+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		// Also clean up on a failed assertion after the deliberate crash.
		s, release, err := openStore(root)
		if err != nil {
			t.Errorf("test cleanup: %v", err)
			return
		}
		defer release()
		saved, err := s.loadRun()
		if err != nil {
			return
		}
		attempts, err := s.attempts(saved)
		if err != nil {
			t.Errorf("test cleanup: %v", err)
			return
		}
		for n := range attempts {
			a := &attempts[n]
			if a.Docker != nil && !a.Docker.Removed {
				if err := recoverDocker(s, saved, Trial{ID: a.TrialID}, a); err != nil {
					t.Errorf("test orphan cleanup: %v", err)
				}
			}
		}
	}()
	run, id := waitForDockerChild(t, root)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	i, exists, err := inspectContainer(installedDocker{}, containerName(run, run.Schedule[0], 1))
	if err != nil || !exists || !i.State.Running || !strings.HasPrefix(i.ID, id) {
		t.Fatalf("test did not leave a running orphan: %v exists=%t inspection=%+v expected ID=%s", err, exists, i, id)
	}
	result, err := ResumeBundle(context.Background(), root)
	if err != nil || !result.Complete || len(result.Attempts) != 2 || result.Attempts[0].Failure != RunnerFailure || result.Attempts[0].Counted || result.Attempts[1].Failure != Timeout {
		t.Fatalf("orphan not recovered: %v %+v", err, result)
	}
	if !result.Attempts[0].Docker.Removed || !strings.Contains(result.Attempts[0].Docker.ProcessesBeforeCleanup, "--child") {
		t.Fatal("orphan descendants were not observed and cleaned")
	}
	assertNoRunContainers(t, result.RunID)
}

func TestDockerOrphanHelper(t *testing.T) {
	root := os.Getenv("AGENTBENCH_DOCKER_ORPHAN_ROOT")
	if root == "" {
		return
	}
	m := dockerFixture(t, "process_tree_timeout")
	m.Policy.TimeoutMS = 10000
	_, err := CreateBundle(context.Background(), m, root)
	if err != nil {
		t.Fatal(err)
	}
}
