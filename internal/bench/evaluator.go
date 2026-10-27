package bench

import (
	"fmt"
	"sort"
)

type calculator struct {
	Operation string `json:"operation"`
	Offset    int    `json:"offset"`
}

func validateCases(cases []TestCase) error {
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Name == "" || seen[c.Name] || c.A < -1000000 || c.A > 1000000 || c.B < -1000000 || c.B > 1000000 || c.Want < -3000000 || c.Want > 3000000 {
			return fmt.Errorf("test cases require unique names and bounded integer values")
		}
		seen[c.Name] = true
	}
	return nil
}

func evaluate(files map[string]string, cases []TestCase) ([]Assertion, error) {
	var calc calculator
	if err := decodeStrict([]byte(files["calculator.json"]), &calc); err != nil {
		return nil, fmt.Errorf("invalid calculator: %w", err)
	}
	if calc.Operation != "add" || calc.Offset < -1000000 || calc.Offset > 1000000 {
		return nil, fmt.Errorf("unsupported calculator operation or offset")
	}
	assertions := make([]Assertion, 0, len(cases))
	for _, c := range cases {
		got := c.A + c.B + calc.Offset
		assertions = append(assertions, Assertion{Name: c.Name, Want: c.Want, Got: got, Passed: got == c.Want})
	}
	return assertions, nil
}

func baselineCheck(files map[string]string) error {
	var tests []TestCase
	if err := decodeStrict([]byte(files["tests.json"]), &tests); err != nil {
		return err
	}
	if len(tests) == 0 {
		return fmt.Errorf("baseline has no repository tests")
	}
	if err := validateCases(tests); err != nil {
		return err
	}
	a, err := evaluate(files, tests)
	if err != nil {
		return err
	}
	if !allPassed(a) {
		return fmt.Errorf("pre-existing repository test failure")
	}
	return nil
}

func allPassed(assertions []Assertion) bool {
	if len(assertions) == 0 {
		return false
	}
	for _, a := range assertions {
		if !a.Passed {
			return false
		}
	}
	return true
}

func extractPatch(before, after map[string]string, allowed []string) (Patch, error) {
	patch := Patch{SchemaVersion: SchemaVersion, Changes: []Change{}}
	paths := map[string]bool{}
	permitted := map[string]bool{}
	for _, p := range allowed {
		permitted[p] = true
	}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		b, bOK := before[p]
		a, aOK := after[p]
		if bOK == aOK && b == a {
			continue
		}
		if !safePath(p) || !permitted[p] {
			return patch, fmt.Errorf("change outside allowed scope: %s", p)
		}
		c := Change{Path: p}
		if bOK {
			v := b
			c.Before = &v
		}
		if aOK {
			v := a
			c.After = &v
		}
		patch.Changes = append(patch.Changes, c)
	}
	return patch, nil
}

func applyPatch(baseline map[string]string, patch Patch, allowed []string) (map[string]string, error) {
	if patch.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported patch schema")
	}
	files := map[string]string{}
	for p, v := range baseline {
		files[p] = v
	}
	permitted := map[string]bool{}
	seen := map[string]bool{}
	for _, p := range allowed {
		permitted[p] = true
	}
	for _, c := range patch.Changes {
		if !safePath(c.Path) || !permitted[c.Path] || seen[c.Path] {
			return nil, fmt.Errorf("invalid or duplicate patch path %q", c.Path)
		}
		seen[c.Path] = true
		v, exists := files[c.Path]
		if (c.Before != nil) != exists || (exists && v != *c.Before) {
			return nil, fmt.Errorf("patch does not match frozen baseline")
		}
		if c.After == nil {
			delete(files, c.Path)
		} else {
			files[c.Path] = *c.After
		}
	}
	return files, nil
}
