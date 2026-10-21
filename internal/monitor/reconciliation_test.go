package monitor

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestRunReconcilesBeforeReadOnlyMonitoring(t *testing.T) {
	cfg := config(t)
	cfg.Interval = time.Hour
	p := store.Proposal{ServiceID: "demo-service", ContainerID: id, Policy: budget.DefaultPolicy()}
	out, err := store.Reserve(context.Background(), cfg.DBPath, p, at)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE reservations SET state='verifying' WHERE reservation_id=?", out.Reservation.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := func(ctx context.Context, target string) (inspection.Result, error) {
		if target != id {
			t.Fatal(target)
		}
		return observation(), nil
	}
	collect := func(context.Context) (inspection.Result, error) {
		var state string
		if err := db.QueryRow("SELECT state FROM reservations").Scan(&state); err != nil || state != "uncertain" {
			t.Fatal(state, err)
		}
		return observation(), nil
	}
	wait := func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if err := loop(ctx, cfg, collect, read, func() time.Time { return at.Add(time.Minute) }, wait, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	h, err := store.InspectBudget(context.Background(), cfg.DBPath, p.ServiceID, p.Policy, at.Add(time.Minute), 20)
	if err != nil || h.Decision.Reason != "unresolved_action" {
		t.Fatal(h, err)
	}
}
