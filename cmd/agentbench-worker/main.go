// agentbench-worker is a fixed fixture process, intended for the scratch image.
// There is intentionally no shell or arbitrary-command interface.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"example.com/agentbench/internal/worker"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(input io.Reader, output io.Writer) error {
	var req worker.Request
	d := json.NewDecoder(io.LimitReader(input, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return err
	}
	if req.SchemaVersion != 1 || len(req.Files) != 2 || req.Attempt < 1 || req.Attempt > 4 {
		return fmt.Errorf("invalid fake request")
	}
	for name, value := range req.Files {
		if name != "calculator.json" && name != "tests.json" {
			return fmt.Errorf("unexpected fixture file")
		}
		if err := os.WriteFile("/workspace/"+name, []byte(value), 0600); err != nil {
			return err
		}
	}
	reply := worker.Reply{Status: "completed"}
	write := func(name, value string) error { return os.WriteFile("/workspace/"+name, []byte(value), 0600) }
	switch req.Behavior {
	case "success", "isolation", "process_tree", "process_tree_timeout":
		if err := write("calculator.json", "{\"operation\":\"add\",\"offset\":0}\n"); err != nil {
			return err
		}
	case "alternate_success":
		if err := write("calculator.json", "{\n  \"offset\": 0,\n  \"operation\": \"add\"\n}\n"); err != nil {
			return err
		}
	case "tamper":
		if err := write("tests.json", "[]\n"); err != nil {
			return err
		}
	case "out_of_scope":
		if err := write("outside.txt", "unauthorized\n"); err != nil {
			return err
		}
	case "malformed":
		_, err := fmt.Fprintln(output, "not JSON")
		return err
	case "interrupted":
		if req.Attempt == 1 {
			if err := write("calculator.json", "{\"operation\":\"add\",\"offset\":999}\n"); err != nil {
				return err
			}
			reply.Status = "interrupted"
		} else if err := write("calculator.json", "{\"operation\":\"add\",\"offset\":0}\n"); err != nil {
			return err
		}
	case "runner_failure", "auth_failure", "provider_outage":
		reply.Status = req.Behavior
	case "failure", "timeout", "evaluator_failure":
	case "output_limit":
		_, err := io.WriteString(output, strings.Repeat("x", (1<<20)+1))
		return err
	default:
		return fmt.Errorf("unsupported fake behavior")
	}
	if req.Behavior == "isolation" {
		reply.Probes = probes()
		for _, passed := range reply.Probes {
			if !passed {
				reply.Status = "isolation_failed"
			}
		}
	}
	if req.Behavior == "process_tree" || req.Behavior == "process_tree_timeout" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(exe, "--child")
		if err := child.Start(); err != nil {
			return err
		}
		reply.ChildPID = child.Process.Pid
		// Do not wait or kill this child. The controller must terminate the
		// complete container, including descendants surviving their parent.
	}
	if req.Behavior == "timeout" || req.Behavior == "process_tree_timeout" {
		if err := json.NewEncoder(output).Encode(worker.Reply{Status: "started", ChildPID: reply.ChildPID}); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	files := map[string]string{}
	entries, err := os.ReadDir("/workspace")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		b, err := os.ReadFile("/workspace/" + entry.Name())
		if err != nil {
			return err
		}
		files[entry.Name()] = string(b)
	}
	reply.Files = files
	return json.NewEncoder(output).Encode(reply)
}

func probes() map[string]bool {
	result := map[string]bool{"non_root": os.Geteuid() == 1000}
	err := os.WriteFile("/root-write-probe", []byte("must fail"), 0600)
	result["root_write_denied"] = err != nil
	_, err = os.Stat("/var/run/docker.sock")
	result["docker_socket_absent"] = os.IsNotExist(err)
	_, err = os.Stat("/inputs/evaluator.json")
	result["trusted_suite_absent"] = os.IsNotExist(err)
	result["host_environment_absent"] = os.Getenv("AGENTBENCH_HOST_SENTINEL") == "" && os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("ANTHROPIC_API_KEY") == ""
	interfaces, err := net.Interfaces()
	isolated := err == nil
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			isolated = false
		}
	}
	result["loopback_only"] = isolated
	status, err := os.ReadFile("/proc/self/status")
	result["no_new_privileges"] = err == nil && strings.Contains(string(status), "NoNewPrivs:\t1")
	result["capabilities_dropped"] = err == nil && strings.Contains(string(status), "CapEff:\t0000000000000000")
	return result
}
