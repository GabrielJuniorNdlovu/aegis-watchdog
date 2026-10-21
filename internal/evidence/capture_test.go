package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func execSQL(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// Only tests seed simulated reservations. No production fake-action command exists.
func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aegis.db")
	a := assessment.Assessment{ContainerID: id, EvaluatedAt: at, CollectedAt: at, Status: "persistent failure", Reason: "check_persistent_failure", Explanation: "Repeated completed checks failed.", Policy: assessment.DefaultPolicy()}
	if err := store.SaveMonitoringResult(context.Background(), path, "demo", inspection.Result{ID: id, Name: "demo", Lifecycle: "running", Health: "unhealthy"}, nil, a); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reserve(context.Background(), path, store.Proposal{ServiceID: "demo", ContainerID: id, Policy: budget.DefaultPolicy(), AssessmentID: 1}, at)
	if err != nil || result.Reservation == nil {
		t.Fatal(result, err)
	}
	return path
}
func normalDocker(t *testing.T, calls *atomic.Int32) *DockerClient {
	return fakeDocker(t, false, func(w http.ResponseWriter, r *http.Request, _ int) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Write(frame("synthetic log\nAuthorization: Bearer AUTH_SECRET\ntoken=TOKEN_SECRET password=PASSWORD_SECRET\n"))
		} else {
			fmt.Fprint(w, body(false, at.Add(-time.Hour)))
		}
	})
}
func record(t *testing.T, path string, id int64) store.CaptureRecord {
	t.Helper()
	rs, err := store.EvidenceRecords(context.Background(), path, id, 1)
	if err != nil || len(rs) != 1 {
		t.Fatal(rs, err)
	}
	return rs[0]
}
func owned(t *testing.T, path string, work func()) {
	t.Helper()
	lock, err := ownership.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	work()
}
func reconcile(t *testing.T, path string) {
	t.Helper()
	owned(t, path, func() {
		if err := ReconcileLocked(context.Background(), path, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCaptureDurabilityRedactionAndNoActionChanges(t *testing.T) {
	path := fixture(t)
	var calls atomic.Int32
	docker := normalDocker(t, &calls)
	before, err := store.InspectBudget(context.Background(), path, "demo", budget.DefaultPolicy(), at, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Real SQLite guards catch accidental changes to action or budget facts.
	execSQL(t, path, `CREATE TRIGGER forbid_action_update BEFORE UPDATE ON reservations BEGIN SELECT RAISE(FAIL,'action changed'); END;
 CREATE TRIGGER forbid_clock_update BEFORE UPDATE ON reservation_clock BEGIN SELECT RAISE(FAIL,'clock changed'); END;`)
	r, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at })
	if err != nil || r.Status != "complete" {
		t.Fatal(r, err)
	}
	dir, _ := directoryPath(path)
	file := filepath.Join(dir, r.BundleName)
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(contents)) != r.Bytes || r.Bytes > DefaultLimits().BundleBytes {
		t.Fatal(r.Bytes)
	}
	for _, secret := range []string{"ENV_SECRET", "PROBE_SECRET", "secret command", "AUTH_SECRET", "TOKEN_SECRET", "PASSWORD_SECRET"} {
		if strings.Contains(string(contents), secret) {
			t.Fatal("sensitive data persisted", secret)
		}
	}
	for _, p := range []string{dir, file} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if p == dir {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal(p, info.Mode())
		}
	}
	repeated, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at.Add(time.Second) })
	if err != nil || repeated.Digest != r.Digest || calls.Load() != 3 {
		t.Fatal(repeated, err, calls.Load())
	}
	after, err := store.InspectBudget(context.Background(), path, "demo", budget.DefaultPolicy(), at, 10)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("budget/action facts changed", err)
	}
	dataBefore, _ := os.ReadFile(path)
	location, err := InspectFile(path, r)
	if err != nil || location != file {
		t.Fatal(location, err)
	}
	dataAfter, _ := os.ReadFile(path)
	if !reflect.DeepEqual(dataBefore, dataAfter) {
		t.Fatal("inspection wrote SQLite")
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectFile(path, r); err == nil {
		t.Fatal("missing file hidden")
	}
	if record(t, path, 1).Status != "complete" {
		t.Fatal("inspection reconciled evidence")
	}
	reconcile(t, path)
	if r = record(t, path, 1); r.Status != "failed" || !contains(r.Issues, "published_evidence_missing_or_invalid") {
		t.Fatal(r)
	}
	reconcile(t, path)
}

func TestPublishedFileSurvivesDatabaseCompletionFailure(t *testing.T) {
	path := fixture(t)
	var calls atomic.Int32
	docker := normalDocker(t, &calls)
	execSQL(t, path, `CREATE TRIGGER fail_completion BEFORE UPDATE ON evidence_captures BEGIN SELECT RAISE(FAIL,'injected completion failure'); END;`)
	if _, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at }); err == nil || !strings.Contains(err.Error(), "SQLite completion failed") {
		t.Fatal(err)
	}
	if r := record(t, path, 1); r.Status != "pending" {
		t.Fatal(r)
	}
	execSQL(t, path, "DROP TRIGGER fail_completion")
	reconcile(t, path)
	r := record(t, path, 1)
	if r.Status != "complete" || r.Digest == "" {
		t.Fatal(r)
	}
	reconcile(t, path)
	if !reflect.DeepEqual(record(t, path, 1), r) {
		t.Fatal("reconciliation is not idempotent")
	}
}

func pending(t *testing.T, path string) store.CaptureRecord {
	t.Helper()
	in := input()
	encoded, _ := json.Marshal(in)
	p, _ := json.Marshal(DefaultLimits())
	r := store.CaptureRecord{ActionID: 1, Status: "pending", StartedAt: at, Input: string(encoded), Limits: string(p)}
	if err := store.BeginEvidence(context.Background(), path, r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestInterruptedAndInvalidPublication(t *testing.T) {
	for _, stage := range []string{"no_file", "temporary", "bad_version", "wrong_identity", "partial_without_issues", "truncated_json"} {
		t.Run(stage, func(t *testing.T) {
			path := fixture(t)
			r := pending(t, path)
			d, err := openDirectory(path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.root.Close()
			_, temp := names(1)
			if stage == "temporary" {
				if err = os.WriteFile(filepath.Join(d.path, temp), []byte(`{"redacted":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if stage != "no_file" && stage != "temporary" {
				b := Bundle{SchemaVersion: 1, Input: input(), Limits: DefaultLimits(), StartedAt: at, EndedAt: at, Status: "complete"}
				switch stage {
				case "bad_version":
					b.SchemaVersion = 2
				case "wrong_identity":
					b.Input.ContainerID = strings.Repeat("b", 64)
				case "partial_without_issues":
					b.Status = "partial"
				}
				body, _ := json.Marshal(b)
				if stage == "truncated_json" {
					body = body[:len(body)/2]
				}
				if err = d.publish(1, body); err != nil {
					t.Fatal(err)
				}
			}
			reconcile(t, path)
			r = record(t, path, 1)
			if r.Status != "failed" {
				t.Fatal(r)
			}
			if _, err = os.Lstat(filepath.Join(d.path, temp)); !os.IsNotExist(err) {
				t.Fatal("abandoned temp retained", err)
			}
			reconcile(t, path)
		})
	}
}

func TestDiskFailuresAndUnexpectedSymlinks(t *testing.T) {
	for _, stage := range []string{"directory_symlink", "temporary_symlink", "final_symlink", "wrong_permissions"} {
		t.Run(stage, func(t *testing.T) {
			path := fixture(t)
			dir, _ := directoryPath(path)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("KEEP"), 0600); err != nil {
				t.Fatal(err)
			}
			if stage == "directory_symlink" {
				if err := os.Symlink(filepath.Dir(outside), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				final, temp := names(1)
				switch stage {
				case "wrong_permissions":
					if err := os.Chmod(dir, 0755); err != nil {
						t.Fatal(err)
					}
				case "temporary_symlink":
					if err := os.Symlink(outside, filepath.Join(dir, temp)); err != nil {
						t.Fatal(err)
					}
				case "final_symlink":
					if err := os.Symlink(outside, filepath.Join(dir, final)); err != nil {
						t.Fatal(err)
					}
				}
			}
			var calls atomic.Int32
			if _, err := Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at }); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			raw, err := os.ReadFile(outside)
			if err != nil || string(raw) != "KEEP" {
				t.Fatal("symlink target touched", string(raw), err)
			}
			if calls.Load() != 0 {
				t.Fatal("Docker queried despite unavailable storage")
			}
		})
	}
	// The real exclusive-create boundary fails when its temporary name exists.
	path := fixture(t)
	d, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	_, temp := names(1)
	if err = os.WriteFile(filepath.Join(d.path, temp), []byte("redacted orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	if _, err = Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at }); err == nil {
		t.Fatal("exclusive create failure hidden")
	}
	if record(t, path, 1).Status != "failed" {
		t.Fatal("failure not persisted")
	}
}

func TestCaptureConcurrencyAndCancellation(t *testing.T) {
	path := fixture(t)
	started := make(chan struct{})
	released := make(chan struct{})
	docker := fakeDocker(t, false, func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			close(released)
		} else {
			fmt.Fprint(w, body(false, at.Add(-time.Hour)))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, err := Capture(ctx, path, 1, docker, DefaultLimits(), func() time.Time { return at })
		if err == nil && (r.Status != "partial" || !contains(r.Issues, "collection_canceled_or_deadline")) {
			err = fmt.Errorf("missing cancellation status: %+v", r)
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not start")
	}
	if _, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at }); err == nil {
		t.Fatal("concurrent capture admitted")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture cancellation blocked")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("log stream not closed")
	}
}

func TestBoundedDatabaseInputAndBundle(t *testing.T) {
	path := fixture(t)
	execSQL(t, path, `UPDATE reservations SET assessment_summary=? WHERE reservation_id=1`, strings.Repeat("X", 100000))
	execSQL(t, path, `UPDATE assessments SET assessment=?`, strings.Repeat("Y", 100000))
	_, as, missing, err := store.EvidenceInput(context.Background(), path, 1)
	if err != nil || len(as) != 0 || !missing {
		t.Fatal(as, missing, err)
	}
	var calls atomic.Int32
	r, err := Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at })
	if err != nil || r.Status != "partial" || !contains(r.Issues, "persisted_assessments_missing_or_omitted") {
		t.Fatal(r, err)
	}
	location, err := InspectFile(path, r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(location)
	if len(body) > MetadataReserve || strings.Contains(string(body), strings.Repeat("X", 100)) {
		t.Fatal("unbounded database metadata persisted")
	}
}

func TestInspectionResponseReadBound(t *testing.T) {
	path := fixture(t)
	docker := fakeDocker(t, false, func(w http.ResponseWriter, r *http.Request, _ int) {
		fmt.Fprint(w, body(false, at.Add(-time.Hour))[:20]+strings.Repeat(" ", 2*MaxDockerResponse))
	})
	r, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at })
	if err != nil || r.Status != "partial" || !contains(r.Issues, "logs_missing") {
		t.Fatal(r, err)
	}
	raw := &countedReader{Reader: strings.NewReader(strings.Repeat("x", 2*MaxDockerResponse))}
	bounded := &responseBody{ReadCloser: io.NopCloser(raw), remaining: MaxDockerResponse}
	if _, err = io.Copy(io.Discard, bounded); err == nil || raw.n != MaxDockerResponse {
		t.Fatal(raw.n, err)
	}
}
