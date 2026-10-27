package bench

import "time"

const SchemaVersion = 1

type FailureClass string

const (
	TaskFailure      FailureClass = "task_failure"
	BaselineFailure  FailureClass = "baseline_failure"
	Timeout          FailureClass = "timeout"
	InvalidPatch     FailureClass = "invalid_patch"
	RunnerFailure    FailureClass = "runner_failure"
	AuthFailure      FailureClass = "auth_failure"
	ProviderOutage   FailureClass = "provider_outage"
	EvaluatorFailure FailureClass = "evaluator_failure"
)

type State string

const (
	Pending           State = "pending"
	Preparing         State = "preparing"
	BaselineValidated State = "baseline_validated"
	Running           State = "running"
	Collecting        State = "collecting"
	Evaluating        State = "evaluating"
	Done              State = "done"
	Failed            State = "failed"
)

// Manifest supports only the built-in arithmetic fixture and fake adapter.
// No command, shell, provider, or arbitrary repository execution is accepted.
type Manifest struct {
	SchemaVersion  int             `json:"schema_version"`
	Task           Task            `json:"task"`
	Configurations []Configuration `json:"configurations"`
	Policy         Policy          `json:"policy"`
}

type Task struct {
	ID           string            `json:"id"`
	Prompt       string            `json:"prompt"`
	Baseline     map[string]string `json:"baseline"`
	AllowedPaths []string          `json:"allowed_paths"`
	Acceptance   []TestCase        `json:"acceptance"`
}

type TestCase struct {
	Name string `json:"name"`
	A    int    `json:"a"`
	B    int    `json:"b"`
	Want int    `json:"want"`
}

type Configuration struct {
	ID       string `json:"id"`
	Adapter  string `json:"adapter"`
	Behavior string `json:"behavior"`
}

type Policy struct {
	Repetitions              int    `json:"repetitions"`
	Seed                     int64  `json:"seed"`
	TimeoutMS                int    `json:"timeout_ms"`
	MaxOutputBytes           int    `json:"max_output_bytes"`
	MaxInfrastructureRetries int    `json:"max_infrastructure_retries"`
	BudgetStatus             string `json:"budget_status"`
}

type Trial struct {
	ID              string `json:"id"`
	ConfigurationID string `json:"configuration_id"`
	Repetition      int    `json:"repetition"`
}

type Run struct {
	SchemaVersion   int       `json:"schema_version"`
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	Mode            string    `json:"mode"`
	Manifest        Manifest  `json:"manifest"`
	BaselineSHA256  string    `json:"baseline_sha256"`
	EvaluatorSHA256 string    `json:"evaluator_sha256"`
	Schedule        []Trial   `json:"schedule"`
}

type CostProvenance string

const (
	ProviderReported     CostProvenance = "provider_reported"
	Estimated            CostProvenance = "estimated"
	SubscriptionIncluded CostProvenance = "subscription_included"
	Unavailable          CostProvenance = "unavailable"
)

type Cost struct {
	Provenance    CostProvenance `json:"provenance"`
	USD           *float64       `json:"usd"`
	PricingSource string         `json:"pricing_source,omitempty"`
	PricingDate   string         `json:"pricing_date,omitempty"`
	Basis         string         `json:"basis"`
}

type Transition struct {
	State State     `json:"state"`
	At    time.Time `json:"at"`
}

type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Want   int    `json:"want"`
	Got    int    `json:"got"`
}

type Attempt struct {
	SchemaVersion   int               `json:"schema_version"`
	TrialID         string            `json:"trial_id"`
	ConfigurationID string            `json:"configuration_id"`
	Repetition      int               `json:"repetition"`
	Number          int               `json:"number"`
	State           State             `json:"state"`
	History         []Transition      `json:"history"`
	Failure         FailureClass      `json:"failure,omitempty"`
	Message         string            `json:"message,omitempty"`
	Counted         bool              `json:"counted"`
	StartedAt       time.Time         `json:"started_at"`
	FinishedAt      *time.Time        `json:"finished_at,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
	Assertions      []Assertion       `json:"assertions,omitempty"`
	ChangedPaths    []string          `json:"changed_paths,omitempty"`
	Artifacts       map[string]string `json:"artifacts"`
	Cost            Cost              `json:"cost"`
}

type Patch struct {
	SchemaVersion int      `json:"schema_version"`
	Changes       []Change `json:"changes"`
}

type Change struct {
	Path   string  `json:"path"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}

type Result struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	Complete      bool      `json:"complete"`
	Attempts      []Attempt `json:"attempts"`
	Summaries     []Summary `json:"summaries"`
}

type Summary struct {
	ConfigurationID        string  `json:"configuration_id"`
	Planned                int     `json:"planned"`
	Samples                int     `json:"samples"`
	Passed                 int     `json:"passed"`
	Failed                 int     `json:"failed"`
	Unresolved             int     `json:"unresolved"`
	InfrastructureAttempts int     `json:"infrastructure_attempts"`
	DurationsMS            []int64 `json:"durations_ms"`
	MinDurationMS          int64   `json:"min_duration_ms"`
	MaxDurationMS          int64   `json:"max_duration_ms"`
	MedianDurationMS       float64 `json:"median_duration_ms"`
}
