package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type store struct{ root string }

// path rejects symlinks in existing bundle components. Candidate files are never
// followed through links, and every artifact path is relative to this bundle.
func (s store) path(rel string) (string, error) {
	if !safePath(rel) {
		return "", fmt.Errorf("unsafe bundle path %q", rel)
	}
	current := s.root
	parts := append([]string{current}, strings.Split(rel, "/")...)
	for i, part := range parts {
		if i > 0 {
			current = filepath.Join(current, part)
		}
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink forbidden in bundle: %s", current)
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(rel)), nil
}

func openStore(root string) (store, func(), error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return store{}, nil, err
	}
	// Existing parents may be user-chosen junctions; links within the bundle
	// itself are rejected. This is a simulation guard, not an OS trust boundary.
	if err := os.MkdirAll(abs, 0700); err != nil {
		return store{}, nil, err
	}
	s := store{root: abs}
	if info, err := os.Lstat(abs); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return store{}, nil, fmt.Errorf("bundle root must be a real directory")
	}
	if info, err := os.Lstat(filepath.Join(abs, ".lock")); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return store{}, nil, fmt.Errorf("bundle lock must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return store{}, nil, err
	}
	release, err := lockBundle(filepath.Join(abs, ".lock"))
	return s, release, err
}

func (s store) write(rel string, data []byte) error {
	p, err := s.path(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, p); err != nil {
		return err
	}
	return nil
}

func (s store) json(rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.write(rel, append(b, '\n'))
}

func (s store) readJSON(rel string, v any) error {
	p, err := s.path(rel)
	if err != nil {
		return err
	}
	b, err := readLimited(p, 8<<20)
	if err != nil {
		return err
	}
	if err = decodeStrict(b, v); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}

func (s store) loadRun() (Run, error) {
	var run Run
	if err := s.readJSON("run.json", &run); err != nil {
		return run, err
	}
	if run.SchemaVersion != SchemaVersion || run.Mode != "synthetic_fixture_simulation" {
		return run, fmt.Errorf("unsupported run schema or mode")
	}
	if err := ValidateManifest(run.Manifest); err != nil {
		return run, err
	}
	var baseline map[string]string
	var evaluator []TestCase
	if err := s.readJSON("inputs/baseline.json", &baseline); err != nil {
		return run, err
	}
	if err := s.readJSON("inputs/evaluator.json", &evaluator); err != nil {
		return run, err
	}
	if digest(baseline) != run.BaselineSHA256 || digest(run.Manifest.Task.Baseline) != run.BaselineSHA256 || digest(evaluator) != run.EvaluatorSHA256 || digest(run.Manifest.Task.Acceptance) != run.EvaluatorSHA256 {
		return run, fmt.Errorf("frozen baseline or evaluator hash mismatch")
	}
	if digest(run.Schedule) != digest(schedule(run.Manifest)) {
		return run, fmt.Errorf("schedule differs from seeded manifest")
	}
	return run, nil
}

func attemptFile(t Trial, number int) string {
	return fmt.Sprintf("attempts/%s/%03d.json", t.ID, number)
}
func attemptDir(t Trial, number int) string { return fmt.Sprintf("attempts/%s/%03d", t.ID, number) }

func (s store) attempts(run Run) ([]Attempt, error) {
	all := []Attempt{}
	for _, t := range run.Schedule {
		missing := false
		closed := false
		for n := 1; n <= run.Manifest.Policy.MaxInfrastructureRetries+1; n++ {
			var a Attempt
			err := s.readJSON(attemptFile(t, n), &a)
			if os.IsNotExist(err) {
				missing = true
				continue
			}
			if err != nil {
				return nil, err
			}
			if missing || closed || a.SchemaVersion != SchemaVersion || a.TrialID != t.ID || a.ConfigurationID != t.ConfigurationID || a.Number != n || a.Repetition != t.Repetition {
				return nil, fmt.Errorf("invalid attempt identity or sequence: %s", attemptFile(t, n))
			}
			if err := validateAttempt(a, t); err != nil {
				return nil, err
			}
			closed = a.Counted || a.Failure == BaselineFailure || (a.State != Done && a.State != Failed)
			all = append(all, a)
		}
	}
	return all, nil
}

func validateAttempt(a Attempt, t Trial) error {
	if len(a.History) == 0 || a.History[0].State != Pending || a.History[len(a.History)-1].State != a.State {
		return fmt.Errorf("invalid attempt history")
	}
	for i := 1; i < len(a.History); i++ {
		if !validTransition(a.History[i-1].State, a.History[i].State) {
			return fmt.Errorf("invalid persisted state transition")
		}
	}
	terminal := a.State == Done || a.State == Failed
	if terminal != (a.FinishedAt != nil) || a.Counted != (a.State == Done || (a.State == Failed && sampleFailure(a.Failure))) {
		return fmt.Errorf("invalid attempt completion or sample accounting")
	}
	if a.State == Failed && !knownFailure(a.Failure) {
		return fmt.Errorf("unknown failure class")
	}
	if a.State != Failed && a.Failure != "" {
		return fmt.Errorf("failure class on non-failed attempt")
	}
	if a.Cost.Provenance != "unavailable" || a.Cost.USD != nil {
		return fmt.Errorf("fake cost must be unavailable, never zero")
	}
	prefix := attemptDir(t, a.Number) + "/"
	for _, p := range a.Artifacts {
		if !safePath(p) || !strings.HasPrefix(p, prefix) {
			return fmt.Errorf("unsafe artifact reference")
		}
	}
	return nil
}

func (s store) snapshot(rel string, files map[string]string) error {
	// Exclusive directory creation makes reuse of a dirty environment impossible.
	p, err := s.path(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(p, 0700); err != nil {
		return fmt.Errorf("fresh workspace required: %w", err)
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !safePath(k) {
			return fmt.Errorf("unsafe snapshot path")
		}
		if err := s.write(rel+"/"+k, []byte(files[k])); err != nil {
			return err
		}
	}
	return nil
}

func (s store) files(rel string, limit int) (map[string]string, error) {
	root, err := s.path(rel)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	total := 0
	err = filepath.WalkDir(root, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("candidate contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("candidate contains a special file")
		}
		relative, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !safePath(name) {
			return fmt.Errorf("candidate contains an unsafe path")
		}
		data, err := readLimited(p, int64(limit-total))
		if err != nil {
			return err
		}
		total += len(data)
		files[name] = string(data)
		if len(files) > 100 {
			return fmt.Errorf("candidate contains too many files")
		}
		return nil
	})
	return files, err
}
