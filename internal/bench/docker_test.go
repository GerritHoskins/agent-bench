package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/agentbench/internal/worker"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validInspection() *containerInspection {
	i := &containerInspection{ID: strings.Repeat("b", 64), Image: testImage}
	i.Config.User = "1000:1000"
	i.Config.WorkingDir = "/workspace"
	i.Config.Env = []string{"PATH=/", "HOME=/nonexistent"}
	i.Config.Entrypoint = []string{"/agentbench-worker"}
	h := &i.HostConfig
	h.NetworkMode = "none"
	h.IpcMode = "none"
	h.ReadonlyRootfs = true
	h.CapDrop = []string{"ALL"}
	h.SecurityOpt = []string{"no-new-privileges=true"}
	h.PidsLimit = 32
	h.Memory = 64 << 20
	h.MemorySwap = 64 << 20
	h.NanoCpus = 500000000
	h.Tmpfs = map[string]string{"/workspace": "rw,noexec,nosuid,nodev,size=16777216,mode=0700,uid=1000,gid=1000"}
	h.LogConfig.Type = "none"
	h.RestartPolicy.Name = "no"
	return i
}

// A small Docker service model exercises lifecycle decisions, not shell quoting
// or the real daemon. The opt-in integration tests verify actual isolation.
type modelDocker struct {
	container                *containerInspection
	calls                    [][]string
	rmFail, killFail, outage bool
	blockStart               bool
	cancelOnStart            context.CancelFunc
	createDeadline           bool
}

func (m *modelDocker) command(ctx context.Context, input []byte, limit int, args ...string) (commandOutput, error) {
	m.calls = append(m.calls, append([]string{}, args...))
	if m.outage {
		return commandOutput{}, fmt.Errorf("injected daemon outage")
	}
	key := strings.Join(args[:2], " ")
	switch key {
	case "image inspect":
		return commandOutput{stdout: []byte(`{"Id":"` + testImage + `","Os":"linux","Config":{"Env":["PATH=/"]}}`)}, nil
	case "container create":
		m.container = validInspection()
		m.container.Config.Labels = map[string]string{}
		for n := 0; n < len(args)-1; n++ {
			if args[n] == "--label" {
				p := strings.SplitN(args[n+1], "=", 2)
				m.container.Config.Labels[p[0]] = p[1]
			}
		}
		if m.createDeadline {
			return commandOutput{}, context.DeadlineExceeded
		}
		return commandOutput{stdout: []byte(m.container.ID)}, nil
	case "container inspect":
		if m.container == nil {
			return commandOutput{}, fmt.Errorf("not found")
		}
		b, _ := json.Marshal(m.container)
		return commandOutput{stdout: b}, nil
	case "container ls":
		if m.container == nil {
			return commandOutput{}, nil
		}
		return commandOutput{stdout: []byte(m.container.ID)}, nil
	case "container top":
		return commandOutput{stdout: []byte("PID PPID ARGS\n1 0 /agentbench-worker\n2 1 /agentbench-worker --child\n")}, nil
	case "container kill":
		if m.killFail {
			return commandOutput{}, fmt.Errorf("injected kill failure")
		}
		m.container.State.Running = false
		return commandOutput{}, nil
	case "container rm":
		if m.rmFail {
			return commandOutput{}, fmt.Errorf("injected removal failure")
		}
		m.container = nil
		return commandOutput{}, nil
	case "container start":
		m.container.State.Running = true
		if m.cancelOnStart != nil {
			m.cancelOnStart()
		}
		if m.blockStart {
			<-ctx.Done()
			return commandOutput{stdout: []byte("{\"status\":\"started\",\"child_pid\":2}\n")}, ctx.Err()
		}
		var req worker.Request
		_ = json.Unmarshal(input, &req)
		req.Files["calculator.json"] = "{\"operation\":\"add\",\"offset\":0}\n"
		b, _ := json.Marshal(worker.Reply{Status: "completed", Files: req.Files})
		m.container.State.Running = false
		return commandOutput{stdout: b}, nil
	default:
		return commandOutput{}, fmt.Errorf("unexpected Docker command %v", args)
	}
}

func modelAttempt(t *testing.T) (store, Run, Trial, *Attempt, *modelDocker, func()) {
	t.Helper()
	s, release, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := fixture(t, "success")
	m.Execution = &Execution{Backend: "docker", Image: testImage}
	run := Run{ID: "test-run", Manifest: m, Mode: "docker_fake_execution"}
	trial := schedule(m)[0]
	a := &Attempt{SchemaVersion: 1, TrialID: trial.ID, ConfigurationID: trial.ConfigurationID, Number: 1, Repetition: 1, Artifacts: map[string]string{}, Cost: Cost{Provenance: Unavailable}}
	a.Docker = &DockerEvidence{Name: containerName(run, trial, 1), Image: testImage}
	model := &modelDocker{container: validInspection()}
	model.container.Config.Labels = containerLabels(run, trial, 1)
	model.container.State.Running = true
	s.docker = model
	return s, run, trial, a, model, release
}

func TestDockerIsolationRejectsWeakenedConfigurations(t *testing.T) {
	if err := verifyIsolation(validInspection()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*containerInspection){
		"network":       func(i *containerInspection) { i.HostConfig.NetworkMode = "bridge" },
		"host PID":      func(i *containerInspection) { i.HostConfig.PidMode = "host" },
		"writable root": func(i *containerInspection) { i.HostConfig.ReadonlyRootfs = false },
		"privileged":    func(i *containerInspection) { i.HostConfig.Privileged = true },
		"root user":     func(i *containerInspection) { i.Config.User = "0" },
		"extra env":     func(i *containerInspection) { i.Config.Env = append(i.Config.Env, "SECRET=unexpected") },
		"socket bind": func(i *containerInspection) {
			i.HostConfig.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
		},
		"no memory limit":      func(i *containerInspection) { i.HostConfig.Memory = 0 },
		"no PID limit":         func(i *containerInspection) { i.HostConfig.PidsLimit = 0 },
		"no CPU limit":         func(i *containerInspection) { i.HostConfig.NanoCpus = 0 },
		"capability added":     func(i *containerInspection) { i.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		"privilege escalation": func(i *containerInspection) { i.HostConfig.SecurityOpt = nil },
		"restart":              func(i *containerInspection) { i.HostConfig.RestartPolicy.Name = "always" },
		"daemon logs":          func(i *containerInspection) { i.HostConfig.LogConfig.Type = "json-file" },
		"unbounded tmpfs":      func(i *containerInspection) { i.HostConfig.Tmpfs["/workspace"] = "rw" },
		"extra command":        func(i *containerInspection) { i.Config.Cmd = []string{"sh"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			i := validInspection()
			mutate(i)
			if err := verifyIsolation(i); err == nil {
				t.Fatal("weakened isolation accepted")
			}
		})
	}
}

func TestDockerCleanupOwnershipAndFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"success", "absent", "wrong owner", "wrong image", "wrong ID", "kill failure", "removal failure", "daemon outage"} {
		t.Run(scenario, func(t *testing.T) {
			s, run, trial, a, model, release := modelAttempt(t)
			defer release()
			switch scenario {
			case "absent":
				model.container = nil
			case "wrong owner":
				model.container.Config.Labels["agentbench.run"] = "someone-else"
			case "wrong image":
				model.container.Image = "sha256:" + strings.Repeat("c", 64)
			case "wrong ID":
				a.Docker.ID = strings.Repeat("d", 64)
			case "kill failure":
				model.killFail = true
			case "removal failure":
				model.rmFail = true
			case "daemon outage":
				model.outage = true
			}
			err := recoverDocker(s, run, trial, a)
			wantSuccess := scenario == "success" || scenario == "absent"
			if (err == nil) != wantSuccess || a.Docker.Removed != wantSuccess {
				t.Fatalf("wrong cleanup outcome: %v %+v", err, a.Docker)
			}
			if strings.HasPrefix(scenario, "wrong") || scenario == "daemon outage" {
				for _, call := range model.calls {
					if call[1] == "kill" || call[1] == "rm" {
						t.Fatal("unowned or unverifiable container modified")
					}
				}
			}
		})
	}
}

func TestDockerCleanupFailureBlocksRetryAndNextTrial(t *testing.T) {
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
	defer release()
	run, err := s.loadRun()
	if err != nil {
		t.Fatal(err)
	}
	run.Mode = "docker_fake_execution"
	run.Manifest.Execution = &Execution{Backend: "docker", Image: testImage}
	if err := s.json("run.json", run); err != nil {
		t.Fatal(err)
	}
	model := &modelDocker{rmFail: true}
	s.docker = model
	result, err := runAndReport(context.Background(), s, run, nil)
	if err == nil {
		t.Fatal("cleanup failure ignored")
	}
	all, loadErr := s.attempts(run)
	if loadErr != nil || len(all) != 1 || all[0].State != Running || all[0].Counted {
		t.Fatalf("advanced after cleanup failure: %v %+v", loadErr, result)
	}
	if err := execute(context.Background(), s, run, nil); err == nil {
		t.Fatal("resume ignored persistent cleanup failure")
	}
	model.rmFail = false
	result, err = runAndReport(context.Background(), s, run, nil)
	if err != nil || !result.Complete || len(result.Attempts) != 3 || result.Attempts[0].Failure != RunnerFailure {
		t.Fatalf("cleanup recovery failed: %v %+v", err, result)
	}
}

func TestDockerTimeoutAndCancellationCleanUpIndependentOfContext(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelParent), func(t *testing.T) {
			s, run, trial, a, model, release := modelAttempt(t)
			defer release()
			model.container = nil
			model.blockStart = true
			a.StartedAt = time.Now()
			if err := checkpoint(s, trial, a, Pending, nil); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			expected := context.DeadlineExceeded
			if cancelParent {
				run.Manifest.Policy.TimeoutMS = 1000
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				model.cancelOnStart = cancel
				expected = context.Canceled
				defer cancel()
			}
			raw := runDocker(ctx, s, run, trial, run.Manifest.Configurations[0], a)
			if !errors.Is(raw.err, expected) || raw.cleanupErr != nil || !a.Docker.Removed || model.container != nil {
				t.Fatalf("context cancellation left a container: %+v %+v", raw, a.Docker)
			}
			if a.Docker.ChildPID != 2 || !strings.Contains(a.Docker.ProcessesBeforeCleanup, "--child") {
				t.Fatal("descendant evidence not collected")
			}
		})
	}
}

func TestPinImageCopiesManifestAndNeverPulls(t *testing.T) {
	m := fixture(t, "success")
	m.Execution = &Execution{Backend: "docker", Image: "agentbench-fake:local"}
	original := m.Execution
	model := &modelDocker{}
	if err := pinDockerImage(context.Background(), &m, model); err != nil {
		t.Fatal(err)
	}
	if m.Execution.Image != testImage || original.Image != "agentbench-fake:local" {
		t.Fatal("image not pinned or caller input mutated")
	}
	if !reflect.DeepEqual(model.calls[0], []string{"image", "inspect", "--format", "{{json .}}", "agentbench-fake:local"}) {
		t.Fatal("unexpected image operation")
	}
}

func TestDockerBoundedOutputCancelsOnSharedStreamOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &boundedStreams{remaining: 8, cancel: cancel}
	_, _ = (streamWriter{capture: capture}).Write([]byte("12345"))
	_, _ = (streamWriter{capture: capture, stderr: true}).Write([]byte("abcdef"))
	if !capture.overflow || capture.stdout.String() != "12345" || capture.stderr.String() != "abc" || ctx.Err() == nil {
		t.Fatal("output cap did not cancel promptly")
	}
}

func TestDockerUncertainCreateDoesNotBecomeTaskTimeout(t *testing.T) {
	s, run, trial, a, model, release := modelAttempt(t)
	defer release()
	model.container = nil
	model.createDeadline = true
	if err := checkpoint(s, trial, a, Pending, nil); err != nil {
		t.Fatal(err)
	}
	raw := runDocker(context.Background(), s, run, trial, run.Manifest.Configurations[0], a)
	if raw.cleanupErr == nil || a.Counted || a.State != Pending || !a.Docker.Removed {
		t.Fatalf("uncertain create allowed advancement: %+v %+v", raw, a)
	}
}

func TestDockerCancelledBeforeCreateStartsNothing(t *testing.T) {
	s, run, trial, a, model, release := modelAttempt(t)
	defer release()
	a.Docker = nil
	model.container = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw := runDocker(ctx, s, run, trial, run.Manifest.Configurations[0], a)
	if !errors.Is(raw.err, context.Canceled) || len(model.calls) != 0 || a.Docker != nil {
		t.Fatal("cancelled execution created a container")
	}
}
