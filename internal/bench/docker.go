package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/agentbench/internal/worker"
)

const dockerManagementTimeout = 20 * time.Second

var errOutputLimit = errors.New("Docker command output limit exceeded")

type commandOutput struct{ stdout, stderr []byte }
type dockerCommander interface {
	command(context.Context, []byte, int, ...string) (commandOutput, error)
}
type installedDocker struct{}

// Both streams share one cap; an overflow cancels the CLI immediately. Killing
// a Docker client does not kill its container: independent cleanup is mandatory.
type boundedStreams struct {
	mu             sync.Mutex
	remaining      int
	stdout, stderr bytes.Buffer
	overflow       bool
	cancel         context.CancelFunc
}
type streamWriter struct {
	capture *boundedStreams
	stderr  bool
}

func (w streamWriter) Write(p []byte) (int, error) {
	c := w.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	keep := n
	if keep > c.remaining {
		keep = c.remaining
		c.overflow = true
		c.cancel()
	}
	if w.stderr {
		c.stderr.Write(p[:keep])
	} else {
		c.stdout.Write(p[:keep])
	}
	c.remaining -= keep
	return n, nil
}
func (installedDocker) command(ctx context.Context, input []byte, limit int, args ...string) (commandOutput, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := &boundedStreams{remaining: limit, cancel: cancel}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = streamWriter{capture: capture}
	cmd.Stderr = streamWriter{capture: capture, stderr: true}
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if capture.overflow {
		err = errOutputLimit
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	return commandOutput{stdout: capture.stdout.Bytes(), stderr: capture.stderr.Bytes()}, err
}

func dockerFor(s store) dockerCommander {
	if s.docker != nil {
		return s.docker
	}
	return installedDocker{}
}
func management(cli dockerCommander, args ...string) (commandOutput, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerManagementTimeout)
	defer cancel()
	return cli.command(ctx, nil, 65536, args...)
}

func pinDockerImage(ctx context.Context, m *Manifest, cli dockerCommander) error {
	if m.Execution == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dockerManagementTimeout)
	defer cancel()
	output, err := cli.command(ctx, nil, 65536, "image", "inspect", "--format", "{{json .}}", m.Execution.Image)
	if err != nil {
		return fmt.Errorf("local Linux fake image unavailable (no pull attempted): %w: %s", err, strings.TrimSpace(string(output.stderr)))
	}
	var image struct {
		ID     string `json:"Id"`
		OS     string `json:"Os"`
		Config struct {
			Env     []string
			Volumes map[string]any
		}
	}
	if err := json.Unmarshal(output.stdout, &image); err != nil {
		return err
	}
	if !imageID.MatchString(image.ID) || image.OS != "linux" || len(image.Config.Env) != 1 || image.Config.Env[0] != "PATH=/" || len(image.Config.Volumes) != 0 {
		return fmt.Errorf("fake image must be a local Linux image with only PATH=/ and no volumes; build docker/fake.Dockerfile")
	}
	// Copy rather than mutate the caller's manifest through its pointer.
	m.Execution = &Execution{Backend: "docker", Image: image.ID}
	return nil
}

type containerInspection struct {
	ID    string `json:"Id"`
	Image string
	State struct {
		Running  bool
		Pid      int
		ExitCode int
		Status   string
	}
	Config struct {
		User       string
		WorkingDir string
		Env        []string
		Labels     map[string]string
		Entrypoint []string
		Cmd        []string
	}
	HostConfig struct {
		NetworkMode    string
		PidMode        string
		IpcMode        string
		ReadonlyRootfs bool
		Privileged     bool
		CapDrop        []string
		CapAdd         []string
		SecurityOpt    []string
		PidsLimit      int64
		Memory         int64
		MemorySwap     int64
		NanoCpus       int64
		Binds          []string
		Mounts         []json.RawMessage
		Devices        []json.RawMessage
		DeviceRequests []json.RawMessage
		PortBindings   map[string]any
		Tmpfs          map[string]string
		AutoRemove     bool
		LogConfig      struct{ Type string }
		RestartPolicy  struct{ Name string }
	}
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
}

type DockerEvidence struct {
	Name                   string               `json:"name"`
	Image                  string               `json:"image"`
	ID                     string               `json:"id,omitempty"`
	IsolationVerified      bool                 `json:"isolation_verified"`
	Inspection             *containerInspection `json:"inspection,omitempty"`
	Started                bool                 `json:"started"`
	Probes                 map[string]bool      `json:"probes,omitempty"`
	ChildPID               int                  `json:"child_pid,omitempty"`
	ProcessesBeforeCleanup string               `json:"processes_before_cleanup,omitempty"`
	Stopped                bool                 `json:"stopped"`
	Removed                bool                 `json:"removed"`
	CleanupError           string               `json:"cleanup_error,omitempty"`
}

func containerName(run Run, t Trial, n int) string {
	return "agentbench-" + digest([]any{run.ID, t.ID, n})[:32]
}
func containerLabels(run Run, t Trial, n int) map[string]string {
	return map[string]string{"agentbench.run": run.ID, "agentbench.trial": t.ID, "agentbench.attempt": strconv.Itoa(n), "agentbench.role": "fake"}
}

func createContainerArgs(run Run, t Trial, a Attempt) []string {
	args := []string{"container", "create", "--name", containerName(run, t, a.Number), "--pull", "never", "--interactive", "--network", "none", "--read-only", "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true", "--pids-limit", "32", "--memory", "64m", "--memory-swap", "64m", "--cpus", "0.5", "--ulimit", "nofile=128:128", "--ipc", "none", "--restart", "no", "--no-healthcheck", "--log-driver", "none", "--workdir", "/workspace", "--env", "PATH=/", "--env", "HOME=/nonexistent", "--tmpfs", "/workspace:rw,noexec,nosuid,nodev,size=16777216,mode=0700,uid=1000,gid=1000", "--entrypoint", "/agentbench-worker"}
	labels := containerLabels(run, t, a.Number)
	for _, key := range []string{"agentbench.run", "agentbench.trial", "agentbench.attempt", "agentbench.role"} {
		args = append(args, "--label", key+"="+labels[key])
	}
	return append(args, run.Manifest.Execution.Image)
}

func inspectContainer(cli dockerCommander, name string) (*containerInspection, bool, error) {
	out, err := management(cli, "container", "inspect", "--format", "{{json .}}", name)
	if err == nil {
		var value containerInspection
		if err := json.Unmarshal(out.stdout, &value); err != nil {
			return nil, false, err
		}
		return &value, true, nil
	}
	// An inspect error could be a daemon outage. Confirm absence with a separate
	// successful exact-name query instead of treating every error as not found.
	listed, listErr := management(cli, "container", "ls", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if listErr != nil {
		return nil, false, fmt.Errorf("cannot confirm container state: %w", listErr)
	}
	if strings.TrimSpace(string(listed.stdout)) != "" {
		return nil, true, fmt.Errorf("container exists but inspect failed: %w", err)
	}
	return nil, false, nil
}

func verifyOwner(inspect *containerInspection, run Run, t Trial, a Attempt) error {
	if inspect.Image != run.Manifest.Execution.Image {
		return fmt.Errorf("container image does not match this attempt")
	}
	for key, value := range containerLabels(run, t, a.Number) {
		if inspect.Config.Labels[key] != value {
			return fmt.Errorf("container ownership mismatch; refusing to modify it")
		}
	}
	if a.Docker.ID != "" && a.Docker.ID != inspect.ID {
		return fmt.Errorf("container identity changed; refusing to modify it")
	}
	return nil
}

func verifyIsolation(i *containerInspection) error {
	h := i.HostConfig
	if h.NetworkMode != "none" || h.PidMode != "" || h.IpcMode != "none" || !h.ReadonlyRootfs || h.Privileged || h.AutoRemove || h.PidsLimit != 32 || h.Memory != 64<<20 || h.MemorySwap != 64<<20 || h.NanoCpus != 500000000 || len(h.Binds) != 0 || len(h.Mounts) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.PortBindings) != 0 || len(h.CapAdd) != 0 || h.LogConfig.Type != "none" || h.RestartPolicy.Name != "no" {
		return fmt.Errorf("container isolation/resource configuration differs from required policy")
	}
	if i.Config.User != "1000:1000" || i.Config.WorkingDir != "/workspace" || len(i.Config.Entrypoint) != 1 || i.Config.Entrypoint[0] != "/agentbench-worker" || len(i.Config.Cmd) != 0 {
		return fmt.Errorf("container process configuration differs from fake policy")
	}
	if len(h.CapDrop) != 1 || !strings.EqualFold(h.CapDrop[0], "ALL") || len(h.SecurityOpt) != 1 || (h.SecurityOpt[0] != "no-new-privileges=true" && h.SecurityOpt[0] != "no-new-privileges") {
		return fmt.Errorf("container privileges differ from required policy")
	}
	if len(i.Config.Env) != 2 {
		return fmt.Errorf("unexpected container environment")
	}
	for _, value := range i.Config.Env {
		if value != "PATH=/" && value != "HOME=/nonexistent" {
			return fmt.Errorf("unexpected container environment")
		}
	}
	if len(h.Tmpfs) != 1 || h.Tmpfs["/workspace"] != "rw,noexec,nosuid,nodev,size=16777216,mode=0700,uid=1000,gid=1000" {
		return fmt.Errorf("unexpected writable mount policy")
	}
	for _, mount := range i.Mounts {
		if mount.Type != "tmpfs" || mount.Destination != "/workspace" || mount.Source != "" {
			return fmt.Errorf("unexpected container mount")
		}
	}
	return nil
}

func saveDocker(s store, t Trial, a *Attempt) error {
	rel := attemptDir(t, a.Number) + "/docker.json"
	if err := s.json(rel, a.Docker); err != nil {
		return err
	}
	a.Artifacts["docker"] = rel
	return s.json(attemptFile(t, a.Number), a)
}

// cleanupContainer never uses a bulk prune, name prefix, host PID, or unrelated
// container. Labels, pinned image and (when known) full ID must all match.
func cleanupContainer(s store, run Run, t Trial, a *Attempt) error {
	if a.Docker == nil {
		return nil
	}
	cli := dockerFor(s)
	i, exists, err := inspectContainer(cli, a.Docker.Name)
	if err != nil {
		return err
	}
	if exists {
		if err := verifyOwner(i, run, t, *a); err != nil {
			return err
		}
		a.Docker.ID = i.ID
		if i.State.Running {
			processes, _ := management(cli, "container", "top", i.ID, "-eo", "pid,ppid,args")
			a.Docker.ProcessesBeforeCleanup = string(processes.stdout)
			if _, err := management(cli, "container", "kill", "--signal", "KILL", i.ID); err != nil {
				return fmt.Errorf("container kill not confirmed: %w", err)
			}
			stopped, stillExists, err := inspectContainer(cli, a.Docker.Name)
			if err != nil || !stillExists || stopped.State.Running {
				return fmt.Errorf("container stop not confirmed: %v", err)
			}
			if err := verifyOwner(stopped, run, t, *a); err != nil {
				return err
			}
		}
		a.Docker.Stopped = true
		if _, err := management(cli, "container", "rm", "--force", i.ID); err != nil {
			return fmt.Errorf("container removal not confirmed: %w", err)
		}
	}
	_, exists, err = inspectContainer(cli, a.Docker.Name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("container still exists after cleanup")
	}
	a.Docker.Stopped = true
	a.Docker.Removed = true
	a.Docker.CleanupError = ""
	return saveDocker(s, t, a)
}

func recoverDocker(s store, run Run, t Trial, a *Attempt) error {
	if err := cleanupContainer(s, run, t, a); err != nil {
		a.Docker.CleanupError = err.Error()
		a.Message = "Docker cleanup failed; execution cannot advance: " + err.Error()
		_ = saveDocker(s, t, a)
		return fmt.Errorf("Docker cleanup failed; refusing to advance: %w", err)
	}
	return nil
}

func runDocker(ctx context.Context, s store, run Run, t Trial, c Configuration, a *Attempt) (result adapterResult) {
	if ctx.Err() != nil {
		return adapterResult{err: ctx.Err()}
	}
	a.Docker = &DockerEvidence{Name: containerName(run, t, a.Number), Image: run.Manifest.Execution.Image}
	// Write the deterministic container intent BEFORE invoking docker create.
	if err := saveDocker(s, t, a); err != nil {
		return adapterResult{err: err, cleanupErr: err}
	}
	defer func() {
		if err := recoverDocker(s, run, t, a); err != nil {
			result.cleanupErr = err
		}
	}()
	cli := dockerFor(s)
	created, err := management(cli, createContainerArgs(run, t, *a)...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The daemon may complete create after its client disappears. Leave
			// this intent unfinished so resume rechecks it before any retry.
			return adapterResult{err: err, cleanupErr: fmt.Errorf("container create outcome uncertain; resume before advancing: %w", err)}
		}
		return adapterResult{err: fmt.Errorf("docker create: %w: %s", err, strings.TrimSpace(string(created.stderr)))}
	}
	a.Docker.ID = strings.TrimSpace(string(created.stdout))
	i, exists, err := inspectContainer(cli, a.Docker.Name)
	if err != nil {
		return adapterResult{err: err}
	}
	if !exists {
		return adapterResult{err: fmt.Errorf("created container disappeared")}
	}
	if err := verifyOwner(i, run, t, *a); err != nil {
		return adapterResult{err: err}
	}
	if err := verifyIsolation(i); err != nil {
		return adapterResult{err: err}
	}
	a.Docker.IsolationVerified = true
	a.Docker.Inspection = i
	if err := saveDocker(s, t, a); err != nil {
		return adapterResult{err: err}
	}
	request := worker.Request{SchemaVersion: 1, Behavior: c.Behavior, Attempt: a.Number, Files: run.Manifest.Task.Baseline}
	input, _ := json.Marshal(request)
	a.Docker.Started = true
	if err := saveDocker(s, t, a); err != nil {
		return adapterResult{err: err}
	}
	execution, cancel := context.WithTimeout(ctx, time.Duration(run.Manifest.Policy.TimeoutMS)*time.Millisecond)
	defer cancel()
	out, err := cli.command(execution, input, run.Manifest.Policy.MaxOutputBytes, "container", "start", "--attach", "--interactive", a.Docker.ID)
	result = adapterResult{stdout: out.stdout, stderr: out.stderr, err: err}
	// Preserve bounded native output separately from normalized completion data.
	for _, raw := range []struct {
		name string
		data []byte
	}{{"docker-stdout.jsonl", out.stdout}, {"docker-stderr.txt", out.stderr}} {
		rel := attemptDir(t, a.Number) + "/" + raw.name
		if saveErr := s.write(rel, raw.data); saveErr != nil {
			result.err = saveErr
			return result
		}
		a.Artifacts[raw.name] = rel
	}
	var reply worker.Reply
	if decodeErr := decodeStrict(out.stdout, &reply); decodeErr != nil {
		if err == nil {
			result.err = fmt.Errorf("malformed Docker worker output: %w", decodeErr)
		}
		return result
	}
	a.Docker.ChildPID = reply.ChildPID
	a.Docker.Probes = reply.Probes
	if err != nil {
		return result
	}
	if reply.Status == "isolation_failed" {
		result.err = fmt.Errorf("worker isolation probe failed")
		return result
	}
	if reply.Status == "runner_failure" || reply.Status == "auth_failure" || reply.Status == "provider_outage" {
		result.failure = FailureClass(reply.Status)
		return result
	}
	if reply.Status != "completed" && reply.Status != "interrupted" {
		result.err = fmt.Errorf("unexpected Docker worker completion")
		return result
	}
	if c.Behavior == "isolation" {
		if len(reply.Probes) != 8 {
			result.err = fmt.Errorf("missing isolation probes")
			return result
		}
		for _, passed := range reply.Probes {
			if !passed {
				result.err = fmt.Errorf("failed isolation probe")
				return result
			}
		}
	}
	if reply.Files == nil {
		result.err = fmt.Errorf("worker returned no candidate files")
		return result
	}
	total := 0
	for path, value := range reply.Files {
		total += len(value)
		if !safePath(path) || total > run.Manifest.Policy.MaxOutputBytes || len(reply.Files) > 100 {
			result.failure = InvalidPatch
			result.err = nil
			return result
		}
	}
	// Replace the retained snapshot with the returned file map only after the
	// protocol has been validated. No tar extraction or docker cp touches host files.
	for path := range run.Manifest.Task.Baseline {
		if _, ok := reply.Files[path]; !ok {
			result.failure = InvalidPatch
			return result
		}
	}
	for path, value := range reply.Files {
		if err := s.write(attemptDir(t, a.Number)+"/workspace/"+path, []byte(value)); err != nil {
			result.err = err
			return result
		}
	}
	result.stdout = []byte("{\"status\":\"completed\"}\n")
	if reply.Status == "interrupted" {
		result.err = ErrInterrupted
	}
	return result
}
