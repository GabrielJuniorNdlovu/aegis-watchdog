package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

var historyTime = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
var historyID = strings.Repeat("a", 64)

func inspectionAPI(t *testing.T, health string, status int) *atomic.Int32 {
	t.Helper()
	var inspections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" || r.URL.RequestURI() != "/v1.52/containers/demo/json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(500)
			return
		}
		inspections.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"SECRET"}`)
			return
		}
		config, state := `{"Env":["TOKEN=SECRET"]}`, `{"Status":"running"}`
		if health != "unknown" {
			config = `{"Env":["TOKEN=SECRET"],"Healthcheck":{"Test":["CMD","secret-command"]}}`
			state = fmt.Sprintf(`{"Status":"running","Health":{"Status":%q,"Log":[{"End":"2026-10-19T10:00:00Z","Output":"SECRET"}]}}`, health)
		}
		fmt.Fprintf(w, `{"Id":%q,"Name":"/demo","Config":%s,"State":%s}`, historyID, config, state)
	}))
	t.Cleanup(server.Close)
	dockerEnv(t, server.URL)
	return &inspections
}

func seedObservation(t *testing.T, path, id, name string, at time.Time) {
	t.Helper()
	r := inspection.Result{ID: id, Name: name, Lifecycle: "running", Health: "unknown"}
	if err := store.Save(context.Background(), path, r, at); err != nil {
		t.Fatal(err)
	}
}

func TestSaveAndHistoryCommands(t *testing.T) {
	for _, health := range []string{"healthy", "unhealthy", "starting", "unknown"} {
		t.Run(health, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observations.db")
			calls := inspectionAPI(t, health, 200)
			var out, errOut bytes.Buffer
			code := run(context.Background(), []string{"inspect", "--container", "demo", "--save", "--db", path}, &out, &errOut)
			if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "Saved observation at:") || !strings.Contains(out.String(), "Health: "+health) {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("save inspected %d times", calls.Load())
			}
			rows, err := store.History(context.Background(), path, historyID, 20)
			if err != nil || len(rows) != 1 || rows[0].Health != health || rows[0].Name != "demo" || rows[0].Lifecycle != "running" || rows[0].ObservedAt.IsZero() {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			if health == "unknown" && !rows[0].CheckTime.IsZero() {
				t.Fatal("unknown check timestamp was invented")
			}
			if health != "unknown" && rows[0].CheckTime.Format(time.RFC3339) != "2026-10-19T10:00:00Z" {
				t.Fatal("check timestamp was not preserved")
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(before, []byte("SECRET")) || bytes.Contains(before, []byte("secret-command")) {
				t.Fatal("secret metadata was stored")
			}
			dockerEnv(t, "invalid-SECRET-Docker-configuration")
			out.Reset()
			errOut.Reset()
			code = run(context.Background(), []string{"history", "--container", historyID, "--limit", "20", "--db", path}, &out, &errOut)
			if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "historical; not current health") || !strings.Contains(out.String(), "Observed at: "+rows[0].ObservedAt.Format(time.RFC3339Nano)) || !strings.Contains(out.String(), "Health: "+health) {
				t.Fatalf("history code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("history modified the database")
			}
		})
	}
}

func TestOrdinaryInspectDoesNotUseSQLite(t *testing.T) {
	t.Chdir(t.TempDir())
	inspectionAPI(t, "healthy", 200)
	for _, args := range [][]string{
		{"inspect", "--container", "demo"},
		{"inspect", "--container", "demo", "--db", ""},
		{"inspect", "--container", "demo", "--db", filepath.Join("missing", "observations.db")},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 0 || errOut.Len() != 0 || strings.Contains(out.String(), "Saved observation") {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	}
	files, err := os.ReadDir(".")
	if err != nil || len(files) != 0 {
		t.Fatalf("ordinary inspect created files: %v %v", files, err)
	}
}

func TestFailedInspectionSavesNothing(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data", "observations.db")
			if existing {
				seedObservation(t, path, historyID, "old", historyTime)
			}
			inspectionAPI(t, "healthy", 404)
			var out, errOut bytes.Buffer
			if code := run(context.Background(), []string{"inspect", "--container", "demo", "--save", "--db", path}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "not found") {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			if existing {
				rows, err := store.History(context.Background(), path, historyID, 20)
				if err != nil || len(rows) != 1 || rows[0].Name != "old" {
					t.Fatalf("failed inspection altered history: %v %v", rows, err)
				}
			} else if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatal("failed inspection created database directory")
			}
		})
	}
}

func TestSaveFailureExit(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	inspectionAPI(t, "healthy", 200)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "--container", "demo", "--save", "--db", filepath.Join(parent, "observations.db")}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "inspection succeeded, but saving failed") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestHistoryArgumentsAndMissingDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	dockerEnv(t, "invalid-Docker-must-not-be-used")
	for _, args := range [][]string{
		{"history"}, {"history", "--container", "demo"}, {"history", "--container", historyID[:12]},
		{"history", "--container", historyID, "--limit", "0"}, {"history", "--container", historyID, "--limit", "-1"},
		{"history", "--container", historyID, "--limit", "1001"}, {"history", "--container", historyID, "--limit", "not-an-integer"},
		{"history", "--container", historyID, "--save"}, {"history", "--container", historyID, "--db", ""},
		{"history", "--container", historyID, "extra"}, {"history", "--container", historyID, "--container", historyID},
		{"inspect", "--container", "demo", "--save", "--db", ""},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("args=%q code=%d out=%q err=%q", args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"history", "--container", historyID}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "database does not exist") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	files, err := os.ReadDir(".")
	if err != nil || len(files) != 0 {
		t.Fatal("invalid arguments or missing history created files")
	}
}

func TestHistoryLimitAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	seedObservation(t, path, historyID, "old", historyTime)
	seedObservation(t, path, historyID, "new", historyTime.Add(time.Minute))
	dockerEnv(t, "invalid-Docker-must-not-be-used")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"history", "--container", historyID, "--limit", "1", "--db", path}, &out, &errOut); code != 0 || errOut.Len() != 0 || strings.Count(out.String(), "Observed at:") != 1 || !strings.Contains(out.String(), "Container: new") || strings.Contains(out.String(), "Container: old") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"history", "--container", strings.Repeat("b", 64), "--db", path}, &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "No saved observations") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"history", "--container", historyID, "--db", path}, brokenOutput{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "cannot write history output") {
		t.Fatalf("code=%d err=%q", code, errOut.String())
	}
}

func TestNewerSchemaCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE sentinel (value TEXT); PRAGMA user_version=3;"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspectionAPI(t, "healthy", 200)
	for _, args := range [][]string{{"history", "--container", historyID, "--db", path}, {"inspect", "--container", "demo", "--save", "--db", path}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "schema version 3") {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("newer database was modified")
	}
}

func TestSavedObservationSurvivesOutputFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observations.db")
	inspectionAPI(t, "unknown", 200)
	var errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "--container", "demo", "--save", "--db", path}, brokenOutput{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "observation was saved") {
		t.Fatalf("code=%d err=%q", code, errOut.String())
	}
	rows, err := store.History(context.Background(), path, historyID, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("committed save lost: %v %v", rows, err)
	}
}
