package bench

import (
	"context"
	"errors"
	"time"
)

var ErrInterrupted = errors.New("execution interrupted; resume this bundle to retry in a fresh workspace")
var ErrUnresolved = errors.New("some trials have no valid sample; inspect infrastructure failures in the report")

type adapterResult struct {
	stdout     []byte
	stderr     []byte
	failure    FailureClass
	err        error
	cleanupErr error // fatal: never retry or advance while a container may survive
}

// fakeExecute performs only hardcoded file writes and waits. It cannot invoke a
// shell, load plugins, make a network request, or execute candidate code.
func fakeExecute(ctx context.Context, s store, workspace string, c Configuration, number int) adapterResult {
	write := func(name, value string) adapterResult {
		if err := s.write(workspace+"/"+name, []byte(value)); err != nil {
			return adapterResult{err: err}
		}
		return adapterResult{stdout: []byte("{\"status\":\"completed\"}\n")}
	}
	select {
	case <-ctx.Done():
		return adapterResult{err: ctx.Err()}
	default:
	}
	switch c.Behavior {
	case "success":
		return write("calculator.json", "{\"operation\":\"add\",\"offset\":0}\n")
	case "alternate_success":
		return write("calculator.json", "{\n  \"offset\": 0,\n  \"operation\": \"add\"\n}\n")
	case "tamper":
		return write("tests.json", "[]\n")
	case "out_of_scope":
		return write("outside.txt", "unauthorized change\n")
	case "timeout":
		<-ctx.Done()
		return adapterResult{stdout: []byte("{\"status\":\"started\"}\n"), err: ctx.Err()}
	case "malformed":
		return adapterResult{stdout: []byte("this is not JSON\n")}
	case "interrupted":
		if number == 1 {
			r := write("calculator.json", "{\"operation\":\"add\",\"offset\":999}\n")
			if r.err == nil {
				r.err = ErrInterrupted
			}
			return r
		}
		return write("calculator.json", "{\"operation\":\"add\",\"offset\":0}\n")
	case "runner_failure":
		return adapterResult{failure: RunnerFailure, stdout: []byte("{\"status\":\"failed\"}\n")}
	case "auth_failure":
		return adapterResult{failure: AuthFailure, stdout: []byte("{\"status\":\"failed\"}\n")}
	case "provider_outage":
		return adapterResult{failure: ProviderOutage, stdout: []byte("{\"status\":\"failed\"}\n")}
	default:
		return adapterResult{stdout: []byte("{\"status\":\"completed\"}\n")}
	}
}

func runFake(ctx context.Context, s store, workspace string, c Configuration, a Attempt, p Policy) adapterResult {
	deadline, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutMS)*time.Millisecond)
	defer cancel()
	return fakeExecute(deadline, s, workspace, c, a.Number)
}
