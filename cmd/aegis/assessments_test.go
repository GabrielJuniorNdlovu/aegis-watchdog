package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestAssessmentCLIReadOnlyWithoutDocker(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	path := filepath.Join(t.TempDir(), "history.db")
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := inspection.Result{ID: strings.Repeat("a", 64), Name: "demo", Lifecycle: "running", Health: "healthy"}
	p := assessment.DefaultPolicy()
	_, a := assessment.Evaluate(assessment.NewState(at, p), assessment.Input{Observation: r, CollectedAt: at}, p, at)
	if err := store.SaveMonitoringResult(ctx, path, "demo", r, nil, a); err != nil {
		t.Fatal(err)
	}
	_, a = assessment.Evaluate(assessment.NewState(at, p), assessment.Input{CollectedAt: at.Add(time.Second), ErrorCategory: "unavailable"}, p, at.Add(time.Second))
	if err := store.SaveMonitoringResult(ctx, path, "demo", inspection.Result{}, errors.New("SECRET"), a); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"assessments", "--container", "demo", "--db", path}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	for _, part := range []string{"Historical assessments; not current health", "Evaluated at:", "Evidence age at evaluation:", "Evidence age now:", "Docker ID: unavailable (collection failed)", "Collection error category: unavailable", assessment.NoAction} {
		if !strings.Contains(out.String(), part) {
			t.Fatal("missing", part, out.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("CLI mutated history")
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("secret leaked")
	}
	out.Reset()
	if code := run(ctx, []string{"assessments", "--container", "other", "--db", path}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "No saved assessments") {
		t.Fatal(code, out.String())
	}
	if code := run(ctx, []string{"assessments", "--container", "demo", "--db", path}, brokenOutput{}, &errOut); code != 1 {
		t.Fatal(code)
	}
}
func TestAssessmentArguments(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	for _, args := range [][]string{
		{"assessments"}, {"assessments", "--container", "demo", "--limit", "0"}, {"assessments", "--container", "demo", "--db", ""},
		{"monitor", "--container", "demo", "--failure-threshold", "0"}, {"monitor", "--container", "demo", "--recovery-threshold", "-1"},
		{"monitor", "--container", "demo", "--max-evidence-age", "0s"}, {"monitor", "--container", "demo", "--max-check-gap", "-1s"},
		{"monitor", "--container", "demo", "--failure-threshold", "0", "--recovery-threshold", "0", "--max-evidence-age", "0s", "--max-check-gap", "0s"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatal(args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"assessments", "--container", "demo", "--db", filepath.Join(t.TempDir(), "missing.db")}, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatal(code, out.String(), errOut.String())
	}
}
func TestEvidenceAge(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if evidenceAge(time.Time{}, at) != "unavailable" || evidenceAge(at, at.Add(time.Second)) != "1s" || !strings.Contains(evidenceAge(at, at.Add(-time.Second)), "future") {
		t.Fatal("incorrect age")
	}
}
