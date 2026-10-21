package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

func evaluated(at time.Time, cause error) (inspection.Result, assessment.Assessment) {
	r := result("unhealthy")
	exit := 1
	r.Evidence = inspection.Evidence{Configured: true, StartedAt: testTime.Add(-time.Hour), Checks: []inspection.Check{{Start: at.Add(-time.Second), End: at.Add(-time.Millisecond), ExitCode: &exit}}}
	category := ""
	if cause != nil {
		category, _ = inspection.FailureDetails(cause)
	}
	_, a := assessment.Evaluate(assessment.NewState(at.Add(-2*time.Second), assessment.DefaultPolicy()), assessment.Input{Observation: r, CollectedAt: at, ErrorCategory: category}, assessment.DefaultPolicy(), at)
	return r, a
}
func TestAssessmentPersistenceOrderingAndReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assessments.db")
	var want []assessment.Assessment
	for _, at := range []time.Time{testTime.Add(time.Second), testTime, testTime.Add(time.Second)} {
		r, a := evaluated(at, nil)
		if err := SaveMonitoringResult(ctx, path, "app", r, nil, a); err != nil {
			t.Fatal(err)
		}
		want = append(want, a)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := AssessmentHistory(ctx, path, "app", 20)
	if err != nil || len(rows) != 3 || !reflect.DeepEqual(rows[0], want[2]) || !reflect.DeepEqual(rows[1], want[0]) || !reflect.DeepEqual(rows[2], want[1]) {
		t.Fatalf("%+v %v", rows, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("history mutated database")
	}
	db := readSQL(t, path)
	if _, err := db.Exec("DELETE FROM assessments"); err == nil {
		t.Fatal("history connection is writable")
	}
	observations, err := History(ctx, path, result("unhealthy").ID, 20)
	if err != nil || len(observations) != 3 || observations[0].Evidence == nil || len(observations[0].Evidence.Checks) != 1 || *observations[0].Evidence.Checks[0].ExitCode != 1 {
		t.Fatalf("evidence lost: %+v %v", observations, err)
	}
	limited, err := AssessmentHistory(ctx, path, "app", 1)
	if err != nil || len(limited) != 1 {
		t.Fatal(limited, err)
	}
	other, err := AssessmentHistory(ctx, path, "other", 20)
	if err != nil || len(other) != 0 {
		t.Fatal(other, err)
	}
}
func TestAssessmentCollectionErrorAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atomic.db")
	cause := errors.New("SECRET")
	r, a := evaluated(testTime, cause)
	if err := SaveMonitoringResult(ctx, path, "app", r, cause, a); err != nil {
		t.Fatal(err)
	}
	rows, err := AssessmentHistory(ctx, path, "app", 20)
	if err != nil || len(rows) != 1 || rows[0].ContainerID != "" || rows[0].Status != "unknown" {
		t.Fatal(rows, err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var nullID bool
	if err := db.QueryRow("SELECT container_id IS NULL FROM assessments").Scan(&nullID); err != nil || !nullID {
		t.Fatal("fabricated identity", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_assessment BEFORE INSERT ON assessments BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{nil, errors.New("SECRET")} {
		r, a := evaluated(testTime.Add(time.Second), cause)
		if err := SaveMonitoringResult(ctx, path, "app", r, cause, a); err == nil {
			t.Fatal("assessment failure ignored")
		}
	}
	observations, err := History(ctx, path, r.ID, 20)
	if err != nil || len(observations) != 0 {
		t.Fatal("orphan observation", observations, err)
	}
	failures, err := ErrorHistory(ctx, path, "app", 20)
	if err != nil || len(failures) != 1 {
		t.Fatal("orphan collection error", failures, err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContainsSecret(bytes) {
		t.Fatal("secret persisted")
	}
}
func bytesContainsSecret(b []byte) bool { return bytes.Contains(b, []byte("SECRET")) }
func TestAssessmentLegacyMigrations(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(schemaV1); err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				if _, err := db.Exec(schemaV2); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO observations(container_id,container_name,observed_at,lifecycle,health) VALUES (?,?,?,'running','unhealthy')`, result("unhealthy").ID, "app", testTime.Format(timestampLayout)); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before, _ := os.ReadFile(path)
			rows, err := AssessmentHistory(ctx, path, "app", 20)
			if err != nil || len(rows) != 0 {
				t.Fatal(rows, err)
			}
			old, err := History(ctx, path, result("unhealthy").ID, 20)
			if err != nil || len(old) != 1 || old[0].Evidence != nil {
				t.Fatal(old, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("legacy read migrated database")
			}
			r, a := evaluated(testTime.Add(time.Second), nil)
			if err := SaveMonitoringResult(ctx, path, "app", r, nil, a); err != nil {
				t.Fatal(err)
			}
			old, err = History(ctx, path, r.ID, 20)
			if err != nil || len(old) != 2 || old[1].Evidence != nil {
				t.Fatal("migration lost or fabricated evidence", old, err)
			}
		})
	}
}
func TestAssessmentRetentionAndIndependentSummary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retention.db")
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		r, a := evaluated(testTime.Add(offset), nil)
		if err := SaveMonitoringResult(ctx, path, "app", r, nil, a); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Prune(ctx, path, testTime)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	rows, err := AssessmentHistory(ctx, path, "app", 20)
	if err != nil || len(rows) != 2 || !rows[1].EvaluatedAt.Equal(testTime) {
		t.Fatal(rows, err)
	}
	db, err := open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM observations"); err != nil {
		t.Fatal(err)
	}
	rows, err = AssessmentHistory(ctx, path, "app", 20)
	if err != nil || len(rows) != 2 || rows[0].EvidenceExitCode == nil || rows[0].Failures != 1 || rows[0].Policy.FailureThreshold != 3 {
		t.Fatal("summary depended on deleted observation", rows, err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<501) INSERT INTO assessments(target,evaluated_at,assessment) SELECT 'app',?,'{}' FROM n`, testTime.Add(-time.Hour).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	n, err = Prune(ctx, path, testTime)
	if err != nil || n != PruneBatch {
		t.Fatal("unbounded prune", n, err)
	}
	n, err = Prune(ctx, path, testTime)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
