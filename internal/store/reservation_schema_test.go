package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReservationMigrationPreservesHistoryAndConstraints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1 + schemaV2 + schemaV3); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO observations(container_id,container_name,observed_at,lifecycle,health) VALUES (?, 'old', ?, 'running','unknown')`, result("unknown").ID, testTime.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, path, result("healthy"), testTime); err != nil {
		t.Fatal(err)
	}
	rows, err := History(ctx, path, result("healthy").ID, 20)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if _, err := db.Exec(`INSERT INTO services VALUES ('api',?); INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) VALUES ('api',?,?,'planned',3,100,10)`, testTime.Format(timestampLayout), result("healthy").ID, testTime.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) SELECT service_id,container_id,reserved_at,'uncertain',3,100,10 FROM reservations`); err == nil {
		t.Fatal("multiple unresolved actions permitted")
	}
	if _, err := db.Exec(`UPDATE reservations SET max_attempts=0`); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if _, err := db.Exec(`UPDATE reservations SET state='expired'`); err == nil {
		t.Fatal("automatic expiration state accepted")
	}
}
