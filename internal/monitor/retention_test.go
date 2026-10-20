package monitor

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestRetentionCadenceWithExplicitTimes(t *testing.T) {
	cfg := config(t)
	cfg.Retention = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := at
	old := observation()
	old.Name = "expired"
	if err := store.Save(ctx, cfg.DBPath, old, at.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	collect := func(context.Context) (inspection.Result, error) { calls++; return observation(), nil }
	wait := func(ctx context.Context, _ time.Duration) error {
		rows, err := store.History(ctx, cfg.DBPath, id, 20)
		if err != nil {
			t.Fatal(err)
		}
		hasExpired := false
		for _, row := range rows {
			if row.Name == "expired" {
				hasExpired = true
			}
		}
		if calls == 1 {
			if hasExpired {
				t.Fatal("initial prune did not run")
			}
			if err := store.Save(ctx, cfg.DBPath, old, at.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			current = at.Add(PruneInterval - time.Nanosecond)
		} else if calls == 2 {
			if !hasExpired {
				t.Fatal("pruned before cadence elapsed")
			}
			current = at.Add(PruneInterval)
		} else {
			if hasExpired {
				t.Fatal("prune did not run at cadence boundary")
			}
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := loop(ctx, cfg, collect, func() time.Time { return current }, wait, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
