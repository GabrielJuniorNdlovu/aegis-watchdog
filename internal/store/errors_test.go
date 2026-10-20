package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectionErrorsAreSeparateAndSanitized(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "errors.db")
	for _, target := range []string{"app", "other", "app"} {
		if err := SaveCollectionError(ctx, path, target, testTime, errors.New(strings.Repeat("SECRET\n", 1000))); err != nil {
			t.Fatal(err)
		}
	}
	failures, err := ErrorHistory(ctx, path, "app", 20)
	if err != nil || len(failures) != 2 {
		t.Fatalf("errors=%v err=%v", failures, err)
	}
	for _, failure := range failures {
		if failure.Category != "request_failed" || len(failure.Explanation) > 256 || strings.Contains(failure.Explanation, "SECRET") || !failure.CollectedAt.Equal(testTime) {
			t.Fatalf("failure=%+v", failure)
		}
	}
	failures, err = ErrorHistory(ctx, path, "app", 1)
	if err != nil || len(failures) != 1 {
		t.Fatalf("limit: %v %v", failures, err)
	}
	failures, err = ErrorHistory(ctx, path, "absent", 20)
	if err != nil || len(failures) != 0 {
		t.Fatalf("empty: %v %v", failures, err)
	}
	rows, err := History(ctx, path, result("healthy").ID, 20)
	if err != nil || len(rows) != 0 {
		t.Fatal("collection errors became application observations")
	}
	if _, err := ErrorHistory(ctx, path, "app", 0); err == nil {
		t.Fatal("accepted invalid limit")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ErrorHistory(ctx, path, "app", 20); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("error history modified database")
	}
}

func TestVersionOneReadCompatibilityAndMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO observations (container_id,container_name,observed_at,lifecycle,health) VALUES (?,?,?,'running','unknown')`, result("unknown").ID, "old", testTime.Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := History(ctx, path, result("unknown").ID, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("v1 history: %v %v", rows, err)
	}
	failures, err := ErrorHistory(ctx, path, "app", 20)
	if err != nil || len(failures) != 0 {
		t.Fatalf("v1 errors: %v %v", failures, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read migrated version 1")
	}
	if err := SaveCollectionError(ctx, path, "app", testTime, errors.New("SECRET")); err != nil {
		t.Fatal(err)
	}
	rows, err = History(ctx, path, result("unknown").ID, 20)
	if err != nil || len(rows) != 1 || rows[0].Name != "old" {
		t.Fatalf("migration lost history: %v %v", rows, err)
	}
	var version int
	if err := readSQL(t, path).QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
