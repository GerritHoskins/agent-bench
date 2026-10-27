package bench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const maxManifestBytes = 1 << 20

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func decodeStrict(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func LoadManifest(filename string) (Manifest, error) {
	var m Manifest
	data, err := readLimited(filename, maxManifestBytes)
	if err != nil {
		return m, err
	}
	if err := decodeStrict(data, &m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	return m, ValidateManifest(m)
}

func ValidateManifest(m Manifest) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", m.SchemaVersion)
	}
	if !identifier.MatchString(m.Task.ID) || strings.TrimSpace(m.Task.Prompt) == "" {
		return fmt.Errorf("task requires a safe ID and a prompt")
	}
	if len(m.Task.Baseline) != 2 {
		return fmt.Errorf("fake fixture baseline must contain only calculator.json and tests.json")
	}
	if _, ok := m.Task.Baseline["calculator.json"]; !ok {
		return fmt.Errorf("missing calculator.json")
	}
	if _, ok := m.Task.Baseline["tests.json"]; !ok {
		return fmt.Errorf("missing tests.json")
	}
	if len(m.Task.AllowedPaths) == 0 {
		return fmt.Errorf("allowed_paths must be explicit")
	}
	seenPaths := map[string]bool{}
	for _, p := range m.Task.AllowedPaths {
		if !safePath(p) || (p != "calculator.json" && p != "tests.json") || seenPaths[p] {
			return fmt.Errorf("unsupported or duplicate allowed path %q", p)
		}
		seenPaths[p] = true
	}
	if !seenPaths["calculator.json"] {
		return fmt.Errorf("calculator.json must be allowed")
	}
	if len(m.Task.Acceptance) == 0 || len(m.Task.Acceptance) > 100 {
		return fmt.Errorf("acceptance requires 1-100 trusted cases")
	}
	if err := validateCases(m.Task.Acceptance); err != nil {
		return err
	}
	if len(m.Configurations) == 0 || len(m.Configurations) > 20 {
		return fmt.Errorf("requires 1-20 configurations")
	}
	seen := map[string]bool{}
	for _, c := range m.Configurations {
		if !identifier.MatchString(c.ID) || seen[c.ID] {
			return fmt.Errorf("unsafe or duplicate configuration ID %q", c.ID)
		}
		seen[c.ID] = true
		if c.Adapter != "fake" {
			return fmt.Errorf("adapter %q is unsupported; only fake is implemented", c.Adapter)
		}
		switch c.Behavior {
		case "success", "alternate_success", "failure", "timeout", "malformed", "interrupted", "tamper", "out_of_scope", "runner_failure", "auth_failure", "provider_outage", "evaluator_failure":
		default:
			return fmt.Errorf("unknown fake behavior %q", c.Behavior)
		}
	}
	p := m.Policy
	if p.Repetitions < 1 || p.Repetitions > 100 || p.TimeoutMS < 1 || p.TimeoutMS > 10000 || p.MaxOutputBytes < 128 || p.MaxOutputBytes > 1<<20 || p.MaxInfrastructureRetries < 0 || p.MaxInfrastructureRetries > 3 {
		return fmt.Errorf("policy limits: repetitions 1-100, timeout_ms 1-10000, max_output_bytes 128-1048576, max_infrastructure_retries 0-3")
	}
	if p.BudgetStatus != "proposed_needs_user_confirmation" {
		return fmt.Errorf("budget_status must remain proposed_needs_user_confirmation")
	}
	return nil
}

func safePath(p string) bool {
	if p == "" || strings.ContainsAny(p, `\:`) || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func readLimited(filename string, limit int64) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", filename, limit)
	}
	return data, nil
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
