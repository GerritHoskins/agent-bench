# AgentBench — offline first slice

A Go CLI that exercises a resumable comparison runner using deterministic fake
adapters and generates an inspectable, local HTML report. **This is harness
validation, not a model benchmark.** AgentBench remains a working name; no public
name or licensing research is claimed in this slice.

No third-party modules, API keys, provider connections, software installation,
Docker daemon, or paid inference are required. Go 1.23+ is required to build. The
CLI accepts only the synthetic calculator fixture and built-in fake behaviors;
it cannot execute repository code, shell commands, plugins, or live agents.

## Quickstart

Run from this directory in PowerShell, bash, or another terminal:

```sh
go build -o dist/agentbench ./cmd/agentbench
go run ./cmd/agentbench demo --out runs/demo
```

Open `runs/demo/report.html` in your browser. Keep the whole bundle beside the
HTML file so its evidence links work. The report embeds its styles, has no
scripts or remote resources, and works without a server. You can copy the bundle
to another directory and rebuild its report without a manifest or provider.

**The full demo intentionally exits 1:** the malformed-output configuration
exhausts its retries and leaves three unresolved samples. The report is still
generated. Expected results are:

| Fake configuration | Passes / samples | Unresolved | Infrastructure attempts |
| --- | --- | --- | --- |
| fixed | 3 / 3 | 0 | 0 |
| fixed-alternate | 3 / 3 | 0 | 0 |
| unchanged | 0 / 3 | 0 | 0 |
| times-out | 0 / 3 | 0 | 0 |
| bad-output | 0 / 0 | 3 | 6 |
| test-tamper | 0 / 3 | 0 | 0 |

Those are fixture outcomes, not measured agent performance. Runtime varies with
filesystem and machine; no demo latency target or speed comparison is claimed.

For a successful run that demonstrates interruption and recovery:

```sh
go run ./cmd/agentbench run --manifest examples/interrupted.json --out runs/recovery
# Stops deliberately, exit 130. Resume in the next command:
go run ./cmd/agentbench resume --bundle runs/recovery
go run ./cmd/agentbench report --bundle runs/recovery
```

`go run` itself may translate the program's nonzero status to exit 1 and print
`exit status 130`; the built binary returns the original status. On Windows,
build an `.exe` explicitly to run it directly:

```powershell
go build -o dist/agentbench.exe ./cmd/agentbench
.\dist\agentbench.exe demo --out runs/another-demo
```

`run` and `demo` require an empty output directory and never overwrite an
existing run. Use a new directory for a fresh comparison. `resume` is idempotent:
it never re-executes a counted trial or resets an exhausted retry budget.
`report` derives results from saved attempt records without running adapters.

Exit statuses of the binary: `0` command finished with all planned samples
resolved (task failures still count as resolved); `1` unresolved samples,
corrupt evidence, I/O errors, or other operational failure; `2` invalid CLI
arguments; `130` interrupted execution. `report` returns `0` when a report was
successfully rebuilt, even if that report describes an incomplete run.

## Fixture and manifest

[`examples/task.json`](examples/task.json) is the full versioned manifest and the
embedded demo source. JSON is deliberately the only accepted format in this
dependency-free slice. Unknown fields, live adapter names, arbitrary baseline
files, unsafe paths, duplicate configuration IDs, and unbounded policy values
are rejected. No YAML parser, arbitrary repository import, or shell evaluator
is implied.

The frozen synthetic repository consists of `calculator.json` and `tests.json`.
The calculator's `offset: 1` introduces a behavioral bug: `2 + 3` returns `6`.
The existing repository test captures that legacy behavior and passes before
the trial; the separate trusted acceptance suite requires correct addition.
Fake success changes the offset to zero. Two success behaviors produce
different patches with the same accepted behavior. The tamper behavior empties
repository tests while retaining the bug; all trusted assertions still fail.

Task, run, attempt, result, cost, assertion, and patch schemas are Go structs in
[`internal/bench/schema.go`](internal/bench/schema.go). The baseline and evaluator
are pinned by SHA-256 of their canonical JSON representation, **not by an
invented Git commit**. The run stores the complete manifest and seeded schedule.

Supported fake behaviors:

| Behavior | Outcome |
| --- | --- |
| `success`, `alternate_success` | Distinct accepted calculator patches |
| `failure` | Unchanged bug → `task_failure` |
| `timeout` | Waits for context deadline → `timeout` |
| `malformed` | Unparseable completion → `runner_failure` |
| `interrupted` | First attempt stops with a dirty workspace; retry succeeds |
| `tamper` | Repository tests changed; trusted checks → `task_failure` |
| `out_of_scope` | Unauthorized file → `invalid_patch` |
| `runner_failure`, `auth_failure`, `provider_outage`, `evaluator_failure` | Injected infrastructure failures; no real provider/authentication used |

## Comparison and recovery contract

The **proposed budget policy needs user confirmation**: equal adapter execution
time, three repetitions per configuration, sequential execution randomized and
interleaved within each repetition, seed 42. The sample manifest uses a 25 ms
adapter deadline to keep the timeout fixture short. Synchronous fixture file I/O
is not preempted. Preparation and evaluation are outside that deadline; reported
duration covers the full attempt. These values are demo settings, not defaults
for future real agents or a statistically reliable ranking.

States are persisted per attempt:

```text
pending → preparing → baseline_validated → running → collecting → evaluating → done
    \________ any unfinished state can end in failed(class) __________________/
```

Only `task_failure`, `timeout`, and `invalid_patch` count as failed samples.
`baseline_failure`, `runner_failure`, `auth_failure`, `provider_outage`, and
`evaluator_failure` do not count. Infrastructure attempts have at most one retry
by default; baseline failures stop their trial immediately. Every attempt is
retained, including exhausted retries. No infrastructure failures are silently
converted into successes or zero-cost samples. Configurations with missing
samples remain unresolved; the report displays the raw sample count and
duration distribution, with no weighted score, confidence claim, or winner.

Each retry creates a new workspace exclusively. Resume marks an unfinished
attempt as an abandoned `runner_failure`, preserves its dirty workspace and raw
evidence, and retries only if the saved budget permits. It does not continue
inside a partially completed environment. Atomic replacement checkpoints and an
OS-held exclusive bundle lock protect against concurrent writers; the OS
releases the lock when a process exits. An interruption after a terminal
checkpoint does not generate a duplicate sample. Failures to seal cleanup
evidence stop execution before the next trial. Evidence corruption, changed
frozen inputs, missing cleanup, or unsafe artifact links fail closed.

Each output bundle contains:

```text
run.json                          frozen manifest, hashes, seeded schedule
inputs/baseline.json              frozen repository files
inputs/evaluator.json             trusted acceptance suite
attempts/<trial>/001.json          attempt, state history, assertions, cost
attempts/<trial>/001/workspace/    retained candidate snapshot
attempts/<trial>/001/patch.json    bounded, scope-checked file changes
attempts/<trial>/001/evaluator-workspace/  baseline plus extracted patch
attempts/<trial>/001/assertions.json       authoritative scoring evidence
attempts/<trial>/001/stdout.jsonl          bounded raw fake output
attempts/<trial>/001/stderr.txt
attempts/<trial>/001/cleanup.json  proof this fake attempt started no processes
results.json                      derived machine-readable summary
report.html                       derived local report with evidence links
```

Only artifacts actually produced by an attempt are present. Patches are JSON
file changes with original content checks, not Git diffs. The evaluator rebuilds
from frozen inputs and applies the patch to a fresh snapshot; candidate
`tests.json` is never authoritative. The Go evaluator only interprets a bounded
integer-addition fixture, never executes candidate code. Every cost is
`{"provenance":"unavailable","usd":null,...}`. The schema reserves
`provider_reported`, `estimated`, and `subscription_included` for future adapters;
no pricing, token usage, model version, or zero-dollar estimate is fabricated.

## Validation

```sh
go test ./... -count=1 -cover
go vet ./...
go build ./cmd/agentbench
```

Tests cover all fake outcomes and eight failure classes, retry accounting,
recovery at every durable state, dirty workspace preservation, pre-existing
baseline failures, distinct isolated patches, tampering, corrupted frozen
inputs, failed/missing cleanup, bounded output, exclusive locking, safe patch
paths, offline escaped HTML and evidence links, CLI exit behavior, and report
rebuilding. Symlink tests explicitly skip if the host disallows symlink creation.

If the Codex Windows sandbox rejects renames in its redirected temporary
directory, use a temporary directory inside this workspace for the test process:

```powershell
New-Item -ItemType Directory -Force runs/test-tmp | Out-Null
$env:TEMP = Join-Path (Get-Location) 'runs/test-tmp'
$env:TMP = $env:TEMP
$env:GOTMPDIR = $env:TEMP
go test ./... -count=1 -cover
```

## Boundaries and retention

This slice is a **controlled file simulation**, with no subprocesses and no
security sandbox. Native Windows fake tests do not demonstrate native Windows
agent execution support. Hashes detect accidental changes, not a malicious host
owner replacing both data and hashes. File writes are synced before checkpoint
replacement; power-loss durability of directory metadata is not guaranteed.
Crash-safe checkpoints cover attempt execution after `run.json` is initialized;
if initial bundle creation is interrupted before then, keep its partial evidence
and choose a new output directory. No automatic deletion or orphan cleanup runs.

All raw evidence and workspaces are retained until manually removed. The report
escapes display text but is not a redaction/export system. This implementation
accepts no credentials and accesses no providers; review any edited task content
before sharing its bundle.

**Stop boundary:** no live provider adapters, subscription authentication,
API-key fallback, paid smoke tests, arbitrary code execution, Docker execution,
package installation, dependency updates, commits, publishing, or deployment.
Before a real agent can execute repository code, a pinned Linux Docker image,
pinned repository commit, container resource/egress limits, credential boundary,
separate evaluator container, process-tree teardown, and live-interface/auth
verification are required. Docker was not available during this implementation;
no container isolation or real-scene performance is claimed. License selection,
public identity checks, real-repository validation, and public release remain
outside this slice.
