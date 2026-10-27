package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"encoding/json"
	"example.com/agentbench/examples"
	"example.com/agentbench/internal/bench"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(command(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func command(ctx context.Context, args []string, out, errOut io.Writer) int {
	usage := func() {
		fmt.Fprintln(out, "AgentBench: offline fake-adapter harness\n\nCommands:\n  demo   --out DIR                         Run the embedded synthetic demo\n  run    --manifest FILE --out DIR         Run a fake-only JSON manifest\n  resume --bundle DIR                      Recover unfinished attempts and continue\n  report --bundle DIR                      Rebuild the local report without running\n\nNo live adapters, credentials, paid calls or repository commands are supported.")
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return 0
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(errOut)
	var output, manifest, bundle string
	switch args[0] {
	case "demo", "run":
		flags.StringVar(&output, "out", "", "new, empty output bundle directory")
		if args[0] == "run" {
			flags.StringVar(&manifest, "manifest", "", "versioned fake-only JSON task manifest")
		}
	case "resume", "report":
		flags.StringVar(&bundle, "bundle", "", "existing run bundle directory")
	default:
		fmt.Fprintf(errOut, "unknown command %q\n", args[0])
		usage()
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errOut, "unexpected positional arguments")
		return 2
	}
	var result bench.Result
	var err error
	switch args[0] {
	case "demo", "run":
		if output == "" {
			fmt.Fprintln(errOut, "--out is required")
			return 2
		}
		var m bench.Manifest
		if args[0] == "demo" {
			err = json.Unmarshal(examples.Demo, &m)
		} else {
			if manifest == "" {
				fmt.Fprintln(errOut, "--manifest is required")
				return 2
			}
			m, err = bench.LoadManifest(manifest)
		}
		if err == nil {
			result, err = bench.CreateBundle(ctx, m, output)
		}
		bundle = output
	case "resume":
		if bundle == "" {
			fmt.Fprintln(errOut, "--bundle is required")
			return 2
		}
		result, err = bench.ResumeBundle(ctx, bundle)
	case "report":
		if bundle == "" {
			fmt.Fprintln(errOut, "--bundle is required")
			return 2
		}
		result, err = bench.ReportBundle(bundle)
	}
	if result.RunID != "" {
		report, _ := filepath.Abs(filepath.Join(bundle, "report.html"))
		fmt.Fprintf(out, "Run %s: resolved=%t\n", result.RunID, result.Complete)
		for _, s := range result.Summaries {
			fmt.Fprintf(out, "  %s: %d/%d passed; %d unresolved; %d infrastructure attempts\n", s.ConfigurationID, s.Passed, s.Samples, s.Unresolved, s.InfrastructureAttempts)
		}
		fmt.Fprintf(out, "Report: %s\n", report)
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		if errors.Is(err, bench.ErrInterrupted) {
			return 130
		}
		return 1
	}
	return 0
}
