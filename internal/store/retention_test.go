package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionCutoffUsesCollectionTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention.db")
	ctx := context.Background()
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond, time.Hour} {
		r := result("healthy")
		r.CheckTime = testTime.Add(-365 * 24 * time.Hour)
		if err := Save(ctx, path, r, testTime.Add(offset)); err != nil {
			t.Fatal(err)
		}
		if err := SaveCollectionError(ctx, path, "app", testTime.Add(offset), errors.New("collection failure")); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := Prune(ctx, path, testTime)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	rows, err := History(ctx, path, result("healthy").ID, 20)
	if err != nil || len(rows) != 3 || !rows[2].ObservedAt.Equal(testTime) {
		t.Fatalf("cutoff observations: %v %v", rows, err)
	}
	failures, err := ErrorHistory(ctx, path, "app", 20)
	if err != nil || len(failures) != 3 || !failures[2].CollectedAt.Equal(testTime) {
		t.Fatalf("cutoff errors: %v %v", failures, err)
	}
}

func TestBoundedPruningPreservesOtherTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.db")
	ctx := context.Background()
	if err := Save(ctx, path, result("unknown"), testTime); err != nil {
		t.Fatal(err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE unresolved_actions (id INTEGER PRIMARY KEY); INSERT INTO unresolved_actions VALUES (1);
        WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<501)
        INSERT INTO observations (container_id,container_name,observed_at,lifecycle,health)
        SELECT ?, 'app', ?, 'running','unknown' FROM n;`, result("unknown").ID, testTime.Add(-time.Hour).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<501)
        INSERT INTO collection_errors(target,collected_at,category,explanation) SELECT 'app',?,'request_failed','bounded test' FROM n`, testTime.Add(-time.Hour).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	deleted, err := Prune(ctx, path, testTime)
	if err != nil || deleted != 2*PruneBatch {
		t.Fatalf("batch deleted=%d err=%v", deleted, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM observations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("observation count=%d err=%v", count, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM collection_errors").Scan(&count); err != nil || count != 1 {
		t.Fatalf("error count=%d err=%v", count, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM unresolved_actions").Scan(&count); err != nil || count != 1 {
		t.Fatal("retention touched unrelated tables")
	}
	deleted, err = Prune(ctx, path, testTime)
	if err != nil || deleted != 2 {
		t.Fatalf("second batch deleted=%d err=%v", deleted, err)
	}
	deleted, err = Prune(ctx, path, testTime)
	if err != nil || deleted != 0 {
		t.Fatalf("completed batch deleted=%d err=%v", deleted, err)
	}
}

func TestPruningRollsBackBothTablesOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	ctx := context.Background()
	if err := Save(ctx, path, result("healthy"), testTime.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := SaveCollectionError(ctx, path, "app", testTime.Add(-time.Hour), errors.New("failure")); err != nil {
		t.Fatal(err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER block_error_pruning BEFORE DELETE ON collection_errors BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := Prune(ctx, path, testTime); err == nil || deleted != 0 {
		t.Fatalf("partial pruning: %d %v", deleted, err)
	}
	rows, err := History(ctx, path, result("healthy").ID, 20)
	if err != nil || len(rows) != 1 {
		t.Fatal("observation delete was not rolled back")
	}
	failures, err := ErrorHistory(ctx, path, "app", 20)
	if err != nil || len(failures) != 1 {
		t.Fatal("error delete was not rolled back")
	}
}
