// Package worker defines the bounded, fake-only container protocol. It has no
// provider, arbitrary command, path, environment, or evaluator inputs.
package worker

type Request struct {
	SchemaVersion int               `json:"schema_version"`
	Behavior      string            `json:"behavior"`
	Attempt       int               `json:"attempt"`
	Files         map[string]string `json:"files"`
}

type Reply struct {
	Status   string            `json:"status"`
	Files    map[string]string `json:"files,omitempty"`
	Probes   map[string]bool   `json:"probes,omitempty"`
	ChildPID int               `json:"child_pid,omitempty"`
}
