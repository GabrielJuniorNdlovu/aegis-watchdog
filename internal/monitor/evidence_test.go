package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/evidence"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestMonitorReconcilesEvidenceBeforeCollection(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "SQLite_failure"}[failure], func(t *testing.T) {
			cfg := config(t)
			result, err := store.Reserve(context.Background(), cfg.DBPath, store.Proposal{ServiceID: "demo", ContainerID: id, Policy: budget.DefaultPolicy()}, at)
			if err != nil || result.Reservation == nil {
				t.Fatal(result, err)
			}
			in, _ := json.Marshal(evidence.Input{ActionID: 1, ServiceID: "demo", ContainerID: id, Policy: budget.DefaultPolicy()})
			limits, _ := json.Marshal(evidence.DefaultLimits())
			if err = store.BeginEvidence(context.Background(), cfg.DBPath, store.CaptureRecord{ActionID: 1, StartedAt: at, Input: string(in), Limits: string(limits)}); err != nil {
				t.Fatal(err)
			}
			if failure {
				db, err := sql.Open("sqlite3", cfg.DBPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TRIGGER fail_evidence BEFORE UPDATE ON evidence_captures BEGIN SELECT RAISE(FAIL,'injected'); END;`)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			collected := false
			collect := func(context.Context) (inspection.Result, error) {
				collected = true
				rows, err := store.EvidenceRecords(ctx, cfg.DBPath, 1, 1)
				if err != nil || len(rows) != 1 || rows[0].Status != "failed" {
					t.Fatal("evidence not reconciled before collection", rows, err)
				}
				return observation(), nil
			}
			err = loop(ctx, cfg, collect, nil, func() time.Time { return at.Add(time.Minute) }, func(context.Context, time.Duration) error { cancel(); return ctx.Err() }, io.Discard, io.Discard)
			if failure {
				if err == nil || !strings.Contains(err.Error(), "evidence reconciliation failed") || collected {
					t.Fatal(collected, err)
				}
			} else if err != nil || !collected {
				t.Fatal(collected, err)
			}
		})
	}
}
