package bench

import (
	"bytes"
	_ "embed"
	"html/template"
)

//go:embed assets/report.html
var reportHTML string

type reportData struct {
	Run    Run
	Result Result
}

func renderReport(s store, run Run, result Result) error {
	t, err := template.New("report").Funcs(template.FuncMap{
		"attemptRecord": func(a Attempt) string { return attemptFile(Trial{ID: a.TrialID}, a.Number) },
	}).Parse(reportHTML)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	if err = t.Execute(&output, reportData{Run: run, Result: result}); err != nil {
		return err
	}
	return s.write("report.html", output.Bytes())
}

// ReportBundle rebuilds derived results from checkpoint records, without running
// any adapter or silently recovering unfinished attempts.
func ReportBundle(root string) (Result, error) {
	s, release, err := openStore(root)
	if err != nil {
		return Result{}, err
	}
	defer release()
	run, err := s.loadRun()
	if err != nil {
		return Result{}, err
	}
	result, err := buildResult(s, run)
	if err != nil {
		return result, err
	}
	if err = s.json("results.json", result); err != nil {
		return result, err
	}
	return result, renderReport(s, run, result)
}
