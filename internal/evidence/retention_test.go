package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func cleanup(t *testing.T, path string, at time.Time) int {
	t.Helper()
	var n int
	owned(t, path, func() {
		var err error
		n, err = CleanupLocked(context.Background(), path, at)
		if err != nil {
			t.Fatal(err)
		}
	})
	return n
}
func TestRetentionProtectionAndInterruptedDeletion(t *testing.T) {
	path := fixture(t)
	var calls atomic.Int32
	r, err := Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	future := at.Add(8 * 24 * time.Hour)
	if n := cleanup(t, path, future); n != 0 {
		t.Fatal("unresolved evidence deleted", n)
	}
	location, err := InspectFile(path, r)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, path, `UPDATE reservations SET state='canceled' WHERE reservation_id=1`)
	if n := cleanup(t, path, at.Add(7*24*time.Hour)); n != 0 {
		t.Fatal("cutoff equality should be retained")
	}
	// Inject a crash after unlink but before final journal completion.
	execSQL(t, path, `CREATE TRIGGER fail_retirement BEFORE UPDATE ON evidence_captures WHEN NEW.deleted_at<>'' BEGIN SELECT RAISE(FAIL,'retirement failure'); END;`)
	owned(t, path, func() {
		if _, err := CleanupLocked(context.Background(), path, future); err == nil {
			t.Fatal("SQLite retirement failure hidden")
		}
	})
	if _, err = os.Stat(location); !os.IsNotExist(err) {
		t.Fatal("file was not unlinked", err)
	}
	r = record(t, path, 1)
	if !r.Deleting || !r.DeletedAt.IsZero() {
		t.Fatal(r)
	}
	execSQL(t, path, "DROP TRIGGER fail_retirement")
	reconcile(t, path)
	r = record(t, path, 1)
	if r.Deleting || r.DeletedAt.IsZero() {
		t.Fatal("deletion not recovered", r)
	}
	reconcile(t, path)
	if n := cleanup(t, path, future); n != 0 {
		t.Fatal(n)
	}
	if r.BundleName == "" || r.Bytes == 0 {
		t.Fatal("historical publication metadata lost")
	}
}
func TestProtectedStorageRefusesCaptureAndKeepsUnknownFiles(t *testing.T) {
	path := fixture(t)
	var calls atomic.Int32
	docker := normalDocker(t, &calls)
	p := DefaultLimits()
	p.TotalBytes = p.BundleBytes
	first, err := Capture(context.Background(), path, 1, docker, p, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	// Simulated second resolved reservation; the first remains unresolved/protected.
	execSQL(t, path, `INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) SELECT service_id,container_id,reserved_at,'canceled',max_attempts,window_ns,cooldown_ns FROM reservations WHERE reservation_id=1`)
	second, err := Capture(context.Background(), path, 2, docker, p, func() time.Time { return at })
	if err == nil || second.Status != "failed" || !contains(second.Issues, "evidence_storage_allowance_exhausted") || calls.Load() != 3 {
		t.Fatal(second, err, calls.Load())
	}
	if _, err = InspectFile(path, first); err != nil {
		t.Fatal("protected evidence damaged", err)
	}
	dir, _ := directoryPath(path)
	unknown := filepath.Join(dir, "operator-notes.txt")
	if err = os.WriteFile(unknown, []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanup(t, path, at.Add(8*24*time.Hour))
	raw, err := os.ReadFile(unknown)
	if err != nil || string(raw) != "KEEP" {
		t.Fatal("unknown file deleted", err)
	}
}
func TestReadAndDeleteRejectReplacementSymlinksAndTraversal(t *testing.T) {
	path := fixture(t)
	var calls atomic.Int32
	r, err := Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	location, err := InspectFile(path, r)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err = os.WriteFile(outside, []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(location); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, location); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectFile(path, r); err == nil {
		t.Fatal("inspection followed symlink")
	}
	r.BundleName = "../private"
	if _, err = InspectFile(path, r); err == nil {
		t.Fatal("inspection accepted traversal")
	}
	execSQL(t, path, `UPDATE reservations SET state='canceled' WHERE reservation_id=1`)
	owned(t, path, func() {
		if _, err := CleanupLocked(context.Background(), path, at.Add(8*24*time.Hour)); err == nil {
			t.Fatal("deletion followed symlink")
		}
	})
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "KEEP" {
		t.Fatal("outside file touched", err)
	}
}
func TestBoundedCleanupAndDirectoryScan(t *testing.T) {
	path := fixture(t)
	// Failed captures with no files are still retired in bounded batches.
	for n := 1; n <= CleanupBatch+1; n++ {
		if n > 1 {
			execSQL(t, path, `INSERT INTO reservations(service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns) SELECT service_id,container_id,reserved_at,'canceled',max_attempts,window_ns,cooldown_ns FROM reservations WHERE reservation_id=1`)
		}
		if n == 1 {
			execSQL(t, path, "UPDATE reservations SET state='canceled'")
		}
		r := pendingRecord(t, path, int64(n))
		r.Status = "failed"
		r.EndedAt = at
		r.Issues = []string{"simulated_interruption"}
		if err := store.UpdateEvidence(context.Background(), path, r); err != nil {
			t.Fatal(err)
		}
	}
	if n := cleanup(t, path, at.Add(8*24*time.Hour)); n != CleanupBatch {
		t.Fatal(n)
	}
	if n := cleanup(t, path, at.Add(8*24*time.Hour)); n != 1 {
		t.Fatal(n)
	}
	d, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	for n := 0; n < MaxEntries; n++ {
		f, err := d.root.OpenFile(fmt.Sprintf("unknown-%d", n), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if _, err = d.usage(); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatal(err)
	}
}
func pendingRecord(t *testing.T, path string, id int64) store.CaptureRecord {
	t.Helper()
	in := input()
	in.ActionID = id
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := json.Marshal(DefaultLimits())
	r := store.CaptureRecord{ActionID: id, Status: "pending", StartedAt: at, Input: string(encoded), Limits: string(limits)}
	if err = store.BeginEvidence(context.Background(), path, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOversizedUnknownFileBlocksAdmissionWithoutDeletion(t *testing.T) {
	path := fixture(t)
	d, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	file, err := d.root.OpenFile("unknown-sparse-file", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(2 * 1024 * 1024 * 1024); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	var calls atomic.Int32
	r, err := Capture(context.Background(), path, 1, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at })
	if err == nil || r.Status != "failed" || calls.Load() != 0 {
		t.Fatal(r, err, calls.Load())
	}
	if info, err := d.root.Stat("unknown-sparse-file"); err != nil || info.Size() != 2*1024*1024*1024 {
		t.Fatal("unknown file changed", err)
	}
}

func TestFullRegistryBlocksCaptureButAllowsBoundedCleanup(t *testing.T) {
	path := fixture(t)
	execSQL(t, path, `UPDATE reservations SET state='canceled';
 WITH RECURSIVE ids(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM ids WHERE n<4097)
 INSERT INTO reservations(reservation_id,service_id,container_id,reserved_at,state,max_attempts,window_ns,cooldown_ns)
 SELECT n,service_id,container_id,reserved_at,'canceled',max_attempts,window_ns,cooldown_ns FROM ids CROSS JOIN reservations WHERE reservation_id=1`)
	r := pendingRecord(t, path, 1)
	r.Status = "failed"
	r.EndedAt = at
	if err := store.UpdateEvidence(context.Background(), path, r); err != nil {
		t.Fatal(err)
	}
	execSQL(t, path, `INSERT INTO evidence_captures(action_id,status,started_at,ended_at,input,limits)
 SELECT reservation_id,'failed',?, ?,json_set(?,'$.ActionID',reservation_id),? FROM reservations WHERE reservation_id BETWEEN 2 AND 4096`, at.Format("2006-01-02T15:04:05.000000000Z"), at.Format("2006-01-02T15:04:05.000000000Z"), r.Input, r.Limits)
	var calls atomic.Int32
	if _, err := Capture(context.Background(), path, 4097, normalDocker(t, &calls), DefaultLimits(), func() time.Time { return at }); err == nil || !strings.Contains(err.Error(), "registry entry limit") || calls.Load() != 0 {
		t.Fatal("full registry admitted capture", err, calls.Load())
	}
	if count := cleanup(t, path, at.Add(8*24*time.Hour)); count != CleanupBatch {
		t.Fatal("full registry prevented cleanup", count)
	}
}
