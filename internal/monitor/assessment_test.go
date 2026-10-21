package monitor

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestMonitorPersistsAssessmentAndStartsFresh(t *testing.T) {
	cfg := config(t)
	cfg.Policy = assessment.DefaultPolicy()
	cfg.Policy.FailureThreshold = 2
	current := at
	for run := 0; run < 2; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		collect := func(context.Context) (inspection.Result, error) {
			calls++
			current = current.Add(time.Second)
			r := observation()
			exit := 1
			r.Evidence = inspection.Evidence{StartedAt: at.Add(-time.Hour), Configured: true, Checks: []inspection.Check{{Start: current.Add(-time.Millisecond), End: current, ExitCode: &exit}}}
			if calls == 3 {
				return inspection.Result{}, errors.New("SECRET")
			}
			return r, nil
		}
		wait := func(ctx context.Context, _ time.Duration) error {
			if calls == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		}
		if err := loop(ctx, cfg, collect, func() time.Time { return current }, wait, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.AssessmentHistory(context.Background(), cfg.DBPath, cfg.Target, 20)
	if err != nil || len(rows) != 6 {
		t.Fatal(rows, err)
	}
	for i, want := range []string{"unknown", "persistent failure", "suspect", "unknown", "persistent failure", "suspect"} {
		if rows[i].Status != want {
			t.Fatalf("%d: %+v", i, rows[i])
		}
	}
	if rows[0].ContainerID != "" || rows[3].ContainerID != "" {
		t.Fatal("error gained identity")
	}
}
