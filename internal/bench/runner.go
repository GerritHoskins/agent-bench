package bench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand"
	"os"
	"sort"
	"time"
)

func schedule(m Manifest) []Trial {
	r := mathrand.New(mathrand.NewSource(m.Policy.Seed))
	trials := make([]Trial, 0, len(m.Configurations)*m.Policy.Repetitions)
	for rep := 1; rep <= m.Policy.Repetitions; rep++ {
		for _, index := range r.Perm(len(m.Configurations)) {
			c := m.Configurations[index]
			trials = append(trials, Trial{ID: fmt.Sprintf("r%03d-%s", rep, c.ID), ConfigurationID: c.ID, Repetition: rep})
		}
	}
	return trials
}

func CreateBundle(ctx context.Context, m Manifest, root string) (Result, error) {
	if err := ValidateManifest(m); err != nil {
		return Result{}, err
	}
	if err := pinDockerImage(ctx, &m, installedDocker{}); err != nil {
		return Result{}, err
	}
	s, release, err := openStore(root)
	if err != nil {
		return Result{}, err
	}
	defer release()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return Result{}, err
	}
	for _, e := range entries {
		if e.Name() != ".lock" {
			return Result{}, fmt.Errorf("output bundle must be empty; use resume for an existing bundle")
		}
	}
	id := make([]byte, 8)
	if _, err = rand.Read(id); err != nil {
		return Result{}, err
	}
	run := Run{SchemaVersion: SchemaVersion, ID: hex.EncodeToString(id), CreatedAt: time.Now().UTC(), Mode: "synthetic_fixture_simulation", Manifest: m, BaselineSHA256: digest(m.Task.Baseline), EvaluatorSHA256: digest(m.Task.Acceptance), Schedule: schedule(m)}
	if m.Execution != nil {
		run.Mode = "docker_fake_execution"
	}
	if err = s.json("inputs/baseline.json", m.Task.Baseline); err != nil {
		return Result{}, err
	}
	if err = s.json("inputs/evaluator.json", m.Task.Acceptance); err != nil {
		return Result{}, err
	}
	if err = s.json("run.json", run); err != nil {
		return Result{}, err
	}
	return runAndReport(ctx, s, run, nil)
}

func ResumeBundle(ctx context.Context, root string) (Result, error) {
	s, release, err := openStore(root)
	if err != nil {
		return Result{}, err
	}
	defer release()
	run, err := s.loadRun()
	if err != nil {
		return Result{}, err
	}
	return runAndReport(ctx, s, run, nil)
}

// hook is used by crash-recovery tests, after a checkpoint reaches durable storage.
type checkpointHook func(Attempt) error

func runAndReport(ctx context.Context, s store, run Run, hook checkpointHook) (Result, error) {
	runErr := execute(ctx, s, run, hook)
	result, err := buildResult(s, run)
	if err != nil {
		return result, errors.Join(runErr, err)
	}
	if err = s.json("results.json", result); err != nil {
		return result, errors.Join(runErr, err)
	}
	if err = renderReport(s, run, result); err != nil {
		return result, errors.Join(runErr, err)
	}
	if runErr == nil && !result.Complete {
		runErr = ErrUnresolved
	}
	return result, runErr
}

func execute(ctx context.Context, s store, run Run, hook checkpointHook) error {
	all, err := s.attempts(run)
	if err != nil {
		return err
	}
	byTrial := map[string][]Attempt{}
	for _, a := range all {
		byTrial[a.TrialID] = append(byTrial[a.TrialID], a)
	}
	configs := map[string]Configuration{}
	for _, c := range run.Manifest.Configurations {
		configs[c.ID] = c
	}
	for _, t := range run.Schedule {
		attempts := byTrial[t.ID]
		for i := range attempts {
			a := &attempts[i]
			if a.State != Done && a.State != Failed {
				if i != len(attempts)-1 {
					return fmt.Errorf("unfinished attempt before a later attempt")
				}
				if a.Docker != nil {
					if err := recoverDocker(s, run, t, a); err != nil {
						return err
					}
				}
				if err := finish(s, t, a, RunnerFailure, "abandoned checkpoint recovered; available artifacts retained and workspace never reused", nil); err != nil {
					return err
				}
			} else if err := checkCleanup(s, t, *a); err != nil {
				return err
			}
		}
		if len(attempts) > 0 {
			last := attempts[len(attempts)-1]
			if last.Counted || last.Failure == BaselineFailure {
				continue
			}
		}
		for n := len(attempts) + 1; n <= run.Manifest.Policy.MaxInfrastructureRetries+1; n++ {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			a := Attempt{SchemaVersion: SchemaVersion, TrialID: t.ID, ConfigurationID: t.ConfigurationID, Repetition: t.Repetition, Number: n, StartedAt: time.Now().UTC(), Artifacts: map[string]string{}, Cost: Cost{Provenance: "unavailable", Basis: "synthetic adapter; no provider usage or monetary charge measured"}}
			if err := checkpoint(s, t, &a, Pending, hook); err != nil {
				return err
			}
			if err := executeAttempt(ctx, s, run, t, configs[t.ConfigurationID], &a, hook); err != nil {
				return err
			}
			if a.Counted || a.Failure == BaselineFailure {
				break
			}
		}
	}
	return nil
}

func executeAttempt(ctx context.Context, s store, run Run, t Trial, c Configuration, a *Attempt, hook checkpointHook) error {
	dir := attemptDir(t, a.Number)
	workspace := dir + "/workspace"
	if err := checkpoint(s, t, a, Preparing, hook); err != nil {
		return err
	}
	if err := s.snapshot(workspace, run.Manifest.Task.Baseline); err != nil {
		return finish(s, t, a, RunnerFailure, err.Error(), hook)
	}
	baseline, err := s.files(workspace, maxManifestBytes)
	if err != nil || digest(baseline) != run.BaselineSHA256 {
		return finish(s, t, a, BaselineFailure, "dirty or unreadable baseline snapshot", hook)
	}
	if err := baselineCheck(baseline); err != nil {
		return finish(s, t, a, BaselineFailure, err.Error(), hook)
	}
	if err := checkpoint(s, t, a, BaselineValidated, hook); err != nil {
		return err
	}
	if err := checkpoint(s, t, a, Running, hook); err != nil {
		return err
	}
	var raw adapterResult
	if run.Manifest.Execution != nil {
		raw = runDocker(ctx, s, run, t, c, a)
	} else {
		raw = runFake(ctx, s, workspace, c, *a, run.Manifest.Policy)
	}
	if raw.cleanupErr != nil {
		return raw.cleanupErr
	}
	limit := run.Manifest.Policy.MaxOutputBytes
	overflow := len(raw.stdout)+len(raw.stderr) > limit
	stdout := raw.stdout
	stderr := raw.stderr
	if len(stdout) > limit {
		stdout = stdout[:limit]
	}
	if len(stderr) > limit-len(stdout) {
		stderr = stderr[:limit-len(stdout)]
	}
	for _, output := range []struct {
		name string
		data []byte
	}{{"stdout.jsonl", stdout}, {"stderr.txt", stderr}} {
		if err := s.write(dir+"/"+output.name, output.data); err != nil {
			return err
		}
		a.Artifacts[output.name] = dir + "/" + output.name
	}
	// Persist raw-output references before returning on an interrupted adapter.
	if err := s.json(attemptFile(t, a.Number), a); err != nil {
		return err
	}
	if errors.Is(raw.err, ErrInterrupted) || ctx.Err() != nil {
		return ErrInterrupted
	}
	if errors.Is(raw.err, context.DeadlineExceeded) {
		return finish(s, t, a, Timeout, "fake adapter reached its wall-clock limit", hook)
	}
	if raw.err != nil {
		return finish(s, t, a, RunnerFailure, raw.err.Error(), hook)
	}
	if raw.failure != "" {
		return finish(s, t, a, raw.failure, "injected fake infrastructure failure", hook)
	}
	if overflow {
		return finish(s, t, a, RunnerFailure, "adapter output limit exceeded; raw output truncated", hook)
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := decodeStrict(raw.stdout, &response); err != nil || response.Status != "completed" {
		return finish(s, t, a, RunnerFailure, "malformed adapter completion output", hook)
	}
	if err := checkpoint(s, t, a, Collecting, hook); err != nil {
		return err
	}
	candidate, err := s.files(workspace, limit)
	if err != nil {
		return finish(s, t, a, InvalidPatch, err.Error(), hook)
	}
	patch, err := extractPatch(baseline, candidate, run.Manifest.Task.AllowedPaths)
	if err != nil {
		return finish(s, t, a, InvalidPatch, err.Error(), hook)
	}
	if err := s.json(dir+"/patch.json", patch); err != nil {
		return err
	}
	a.Artifacts["patch"] = dir + "/patch.json"
	for _, change := range patch.Changes {
		a.ChangedPaths = append(a.ChangedPaths, change.Path)
	}
	if err := checkpoint(s, t, a, Evaluating, hook); err != nil {
		return err
	}
	// Rebuild from the immutable input, never from the candidate workspace.
	var frozen map[string]string
	var suite []TestCase
	if err := s.readJSON("inputs/baseline.json", &frozen); err != nil {
		return finish(s, t, a, EvaluatorFailure, err.Error(), hook)
	}
	if err := s.readJSON("inputs/evaluator.json", &suite); err != nil {
		return finish(s, t, a, EvaluatorFailure, err.Error(), hook)
	}
	if digest(frozen) != run.BaselineSHA256 || digest(suite) != run.EvaluatorSHA256 {
		return finish(s, t, a, EvaluatorFailure, "trusted input hash mismatch", hook)
	}
	fresh, err := applyPatch(frozen, patch, run.Manifest.Task.AllowedPaths)
	if err != nil {
		return finish(s, t, a, InvalidPatch, err.Error(), hook)
	}
	if err := s.snapshot(dir+"/evaluator-workspace", fresh); err != nil {
		return finish(s, t, a, EvaluatorFailure, err.Error(), hook)
	}
	if c.Behavior == "evaluator_failure" {
		return finish(s, t, a, EvaluatorFailure, "injected trusted evaluator failure", hook)
	}
	assertions, err := evaluate(fresh, suite)
	if err != nil {
		return finish(s, t, a, TaskFailure, err.Error(), hook)
	}
	a.Assertions = assertions
	if err := s.json(dir+"/assertions.json", assertions); err != nil {
		return err
	}
	a.Artifacts["assertions"] = dir + "/assertions.json"
	if !allPassed(assertions) {
		return finish(s, t, a, TaskFailure, "trusted acceptance assertions failed", hook)
	}
	return finish(s, t, a, "", "trusted acceptance assertions passed", hook)
}

func checkpoint(s store, t Trial, a *Attempt, state State, hook checkpointHook) error {
	if !validTransition(a.State, state) {
		return fmt.Errorf("invalid transition %s -> %s", a.State, state)
	}
	a.State = state
	a.History = append(a.History, Transition{State: state, At: time.Now().UTC()})
	if err := s.json(attemptFile(t, a.Number), a); err != nil {
		return err
	}
	if hook != nil {
		return hook(*a)
	}
	return nil
}

func validTransition(from, to State) bool {
	if from == "" {
		return to == Pending
	}
	if from == Done || from == Failed {
		return false
	}
	if to == Failed {
		return true
	}
	next := map[State]State{Pending: Preparing, Preparing: BaselineValidated, BaselineValidated: Running, Running: Collecting, Collecting: Evaluating, Evaluating: Done}
	return next[from] == to
}

func sampleFailure(f FailureClass) bool { return f == TaskFailure || f == Timeout || f == InvalidPatch }
func knownFailure(f FailureClass) bool {
	switch f {
	case TaskFailure, BaselineFailure, Timeout, InvalidPatch, RunnerFailure, AuthFailure, ProviderOutage, EvaluatorFailure:
		return true
	}
	return false
}

type cleanupEvidence struct {
	Mode              string `json:"mode"`
	ProcessesStarted  int    `json:"processes_started,omitempty"`
	WorkspaceRetained bool   `json:"workspace_retained"`
	ContainerName     string `json:"container_name,omitempty"`
	ContainerRemoved  bool   `json:"container_removed,omitempty"`
}

func finish(s store, t Trial, a *Attempt, failure FailureClass, message string, hook checkpointHook) error {
	// Docker cleanup must finish before any terminal checkpoint or retry.
	cleanup := attemptDir(t, a.Number) + "/cleanup.json"
	evidence := cleanupEvidence{Mode: "built_in_fake_only", WorkspaceRetained: true}
	if a.Docker != nil {
		if !a.Docker.Removed || a.Docker.CleanupError != "" {
			return fmt.Errorf("Docker cleanup not confirmed; refusing to finish attempt")
		}
		evidence.Mode = "docker_fake"
		evidence.ContainerName = a.Docker.Name
		evidence.ContainerRemoved = true
	}
	if err := s.json(cleanup, evidence); err != nil {
		return err
	}
	a.Artifacts["cleanup"] = cleanup
	a.Failure = failure
	a.Message = message
	a.Counted = failure == "" || sampleFailure(failure)
	now := time.Now().UTC()
	a.FinishedAt = &now
	a.DurationMS = now.Sub(a.StartedAt).Milliseconds()
	state := Done
	if failure != "" {
		state = Failed
	}
	return checkpoint(s, t, a, state, hook)
}

func checkCleanup(s store, t Trial, a Attempt) error {
	var evidence cleanupEvidence
	if err := s.readJSON(attemptDir(t, a.Number)+"/cleanup.json", &evidence); err != nil {
		return fmt.Errorf("cleanup not confirmed; refusing to advance: %w", err)
	}
	valid := evidence.Mode == "built_in_fake_only" && evidence.ProcessesStarted == 0 && a.Docker == nil
	if a.Docker != nil {
		valid = evidence.Mode == "docker_fake" && evidence.ContainerName == a.Docker.Name && evidence.ContainerRemoved && a.Docker.Removed && a.Docker.CleanupError == ""
	}
	if !valid || !evidence.WorkspaceRetained {
		return fmt.Errorf("invalid cleanup evidence; refusing to advance")
	}
	return nil
}

func buildResult(s store, run Run) (Result, error) {
	all, err := s.attempts(run)
	result := Result{SchemaVersion: SchemaVersion, RunID: run.ID, Complete: true, Attempts: all, Summaries: []Summary{}}
	if err != nil {
		return result, err
	}
	for _, a := range all {
		if a.State == Done || a.State == Failed {
			if err := checkCleanup(s, Trial{ID: a.TrialID}, a); err != nil {
				return result, err
			}
		}
		for _, rel := range a.Artifacts {
			p, err := s.path(rel)
			if err != nil {
				return result, err
			}
			if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
				return result, fmt.Errorf("missing or non-regular evidence artifact %s", rel)
			}
		}
	}
	for _, c := range run.Manifest.Configurations {
		summary := Summary{ConfigurationID: c.ID, Planned: run.Manifest.Policy.Repetitions, DurationsMS: []int64{}}
		for _, a := range all {
			if a.ConfigurationID != c.ID {
				continue
			}
			if a.Counted {
				summary.Samples++
				summary.DurationsMS = append(summary.DurationsMS, a.DurationMS)
				if a.State == Done {
					summary.Passed++
				} else {
					summary.Failed++
				}
			} else {
				summary.InfrastructureAttempts++
			}
		}
		summary.Unresolved = summary.Planned - summary.Samples
		if summary.Unresolved > 0 {
			result.Complete = false
		}
		if len(summary.DurationsMS) > 0 {
			sorted := append([]int64{}, summary.DurationsMS...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			summary.MinDurationMS = sorted[0]
			summary.MaxDurationMS = sorted[len(sorted)-1]
			summary.MedianDurationMS = float64(sorted[len(sorted)/2])
			if len(sorted)%2 == 0 {
				summary.MedianDurationMS = float64(sorted[len(sorted)/2-1]+sorted[len(sorted)/2]) / 2
			}
		}
		result.Summaries = append(result.Summaries, summary)
	}
	return result, nil
}
