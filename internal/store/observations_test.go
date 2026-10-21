package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

var testTime = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func result(health string) inspection.Result {
	return inspection.Result{ID: strings.Repeat("a", 64), Name: "app", Lifecycle: "running", Health: health, Explanation: "SECRET must never be saved"}
}

func readSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSaveAndReopen(t *testing.T) {
	for _, health := range []string{"healthy", "unhealthy", "starting", "unknown"} {
		t.Run(health, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "observations ?#1.db")
			r := result(health)
			if health != "unknown" {
				r.CheckTime = testTime.Add(-time.Minute).In(time.FixedZone("offset", 7200))
			}
			at := testTime.In(time.FixedZone("offset", 7200))
			if err := Save(context.Background(), path, r, at); err != nil {
				t.Fatal(err)
			}
			// Save and History each close their own connection: this read reopens the file.
			rows, err := History(context.Background(), path, r.ID, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows=%v", rows)
			}
			got := rows[0]
			if got.ContainerID != r.ID || got.Name != r.Name || got.Lifecycle != r.Lifecycle || got.Health != health || !got.CheckTime.Equal(r.CheckTime) || !got.ObservedAt.Equal(at) || got.ObservedAt.Location() != time.UTC {
				t.Fatalf("observation=%+v", got)
			}
			if err := Save(context.Background(), path, r, at); err != nil {
				t.Fatal(err)
			}
			rows, err = History(context.Background(), path, r.ID, 20)
			if err != nil || len(rows) != 2 {
				t.Fatalf("repeated save: rows=%v err=%v", rows, err)
			}
			db := readSQL(t, path)
			var version, nullChecks int
			var observed, journal string
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
				t.Fatalf("version=%d err=%v", version, err)
			}
			if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
				t.Fatalf("journal=%s err=%v", journal, err)
			}
			if err := db.QueryRow("SELECT observed_at FROM observations LIMIT 1").Scan(&observed); err != nil || observed != "2026-10-20T12:00:00.000000000Z" {
				t.Fatalf("observed_at=%s err=%v", observed, err)
			}
			if err := db.QueryRow("SELECT count(*) FROM observations WHERE check_time IS NULL").Scan(&nullChecks); err != nil {
				t.Fatal(err)
			}
			if health == "unknown" && nullChecks != 2 {
				t.Fatal("absent timestamp was not stored as NULL")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("database permissions: %v %v", info, err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(content, []byte("SECRET")) {
				t.Fatal("explanation was stored")
			}
		})
	}
}

func TestOrderingLimitAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	r := result("healthy")
	for _, item := range []struct {
		name string
		at   time.Time
	}{
		{"old", testTime.Add(-time.Hour)},
		{"fraction", testTime.Add(time.Nanosecond)},
		{"tie-first", testTime},
		{"tie-last", testTime},
	} {
		r.Name = item.name
		if err := Save(context.Background(), path, r, item.at); err != nil {
			t.Fatal(err)
		}
	}
	other := r
	other.ID = strings.Repeat("b", 64)
	if err := Save(context.Background(), path, other, testTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := History(context.Background(), path, r.ID, MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, row.Name)
	}
	if want := []string{"fraction", "tie-last", "tie-first", "old"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names=%v, want %v", names, want)
	}
	rows, err = History(context.Background(), path, r.ID, 1)
	if err != nil || len(rows) != 1 || rows[0].Name != "fraction" {
		t.Fatalf("limit: rows=%v err=%v", rows, err)
	}
	rows, err = History(context.Background(), path, other.ID, 20)
	if err != nil || len(rows) != 1 || rows[0].Name != r.Name {
		t.Fatalf("other identity: rows=%v err=%v", rows, err)
	}
	rows, err = History(context.Background(), path, strings.Repeat("c", 64), 20)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty history: %v %v", rows, err)
	}
	for _, limit := range []int{-1, 0, MaxLimit + 1} {
		if _, err := History(context.Background(), path, r.ID, limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := History(context.Background(), path, "app", 20); err == nil {
		t.Fatal("accepted a name for history")
	}
}

func TestMissingDatabaseAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "history.db")
	if _, err := History(context.Background(), path, result("unknown").ID, 20); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("history created a directory")
	}
	if err := Save(context.Background(), path, result("unknown"), testTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := History(context.Background(), path, result("unknown").ID, 20); err != nil {
		t.Fatal(err)
	}
	db := readSQL(t, path)
	if _, err := db.Exec("DELETE FROM observations"); err == nil {
		t.Fatal("history connection permits writes")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("history modified the database")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("history created sidecar files: %v %v", files, err)
	}
}

func TestSchemaVersionsDoNotModifyDatabase(t *testing.T) {
	for _, version := range []int{0, 6} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "version.db")
			db, err := open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			statement := "CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('keep');"
			if version == 6 {
				statement += "PRAGMA user_version=6;"
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := History(context.Background(), path, result("unknown").ID, 20); err == nil || !strings.Contains(err.Error(), "schema version") {
				t.Fatalf("history error=%v", err)
			}
			if version == 6 {
				if err := Save(context.Background(), path, result("unknown"), testTime); err == nil || !strings.Contains(err.Error(), "schema version 6") {
					t.Fatalf("save error=%v", err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("unsupported schema was modified")
			}
		})
	}
}

func TestFailedSaveRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	r := result("invalid")
	if err := Save(context.Background(), path, r, testTime); err == nil {
		t.Fatal("invalid observation saved")
	}
	db := readSQL(t, path)
	var tables, version int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='observations'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || version != 0 {
		t.Fatal("failed save committed a partial migration")
	}
	if err := Save(context.Background(), path, result("healthy"), testTime); err != nil {
		t.Fatal(err)
	}
	if err := Save(context.Background(), path, r, testTime); err == nil {
		t.Fatal("invalid observation saved to existing database")
	}
	rows, err := History(context.Background(), path, r.ID, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("existing observation lost: %v %v", rows, err)
	}
}

func TestLockWaitAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	r := result("healthy")
	if err := Save(context.Background(), path, r, testTime); err != nil {
		t.Fatal(err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = Save(ctx, path, r, testTime)
	if err == nil || !strings.Contains(err.Error(), "locked") || ctx.Err() != nil {
		t.Fatalf("bounded busy wait error=%v ctx=%v", err, ctx.Err())
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := Save(canceled, path, r, testTime); err == nil {
		t.Fatal("canceled save succeeded")
	}
	if _, err := History(canceled, path, r.ID, 20); err == nil {
		t.Fatal("canceled history succeeded")
	}
}

func TestInvalidDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := History(context.Background(), path, result("healthy").ID, 20); err == nil {
		t.Fatal("corrupt database accepted")
	}
	if err := Save(context.Background(), path, result("healthy"), testTime); err == nil {
		t.Fatal("corrupt database accepted for saving")
	}
	if err := Save(context.Background(), "", result("healthy"), testTime); err == nil {
		t.Fatal("empty path accepted")
	}
}
