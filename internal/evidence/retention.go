package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

// CleanupLocked retires at most CleanupBatch resolved captures per pass. It must
// share the directory ownership lock with capture and action reconciliation.
func CleanupLocked(ctx context.Context, path string, at time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultLimits().Deadline)
	defer cancel()
	records, err := store.EvidenceRecords(ctx, path, 0, MaxEntries+1)
	if err != nil {
		return 0, err
	}
	if len(records) > MaxEntries {
		return 0, errors.New("evidence registry entry limit reached")
	}
	if len(records) == 0 {
		return 0, nil
	}
	d, err := openDirectory(path, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if d != nil {
		defer d.root.Close()
	}
	count := 0
	for _, r := range records {
		if count == CleanupBatch {
			break
		}
		if r.Protected || r.Status == "pending" {
			continue
		}
		var p Limits
		if json.Unmarshal([]byte(r.Limits), &p) != nil || p.Validate() != nil {
			return count, errors.New("invalid retained evidence limits")
		}
		if !r.Deleting && !r.StartedAt.Before(at.Add(-p.Retention)) {
			continue
		}
		if !r.Deleting {
			r.Deleting = true
			if err = store.UpdateEvidence(ctx, path, r); err != nil {
				return count, err
			}
		}
		if err = finishDeletion(ctx, path, d, r, at); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
func finishDeletion(ctx context.Context, path string, d *directory, r store.CaptureRecord, at time.Time) error {
	if r.Protected {
		return errors.New("refusing to delete evidence for an unresolved action")
	}
	final, temp := names(r.ActionID)
	if r.BundleName != "" && r.BundleName != final {
		return errors.New("unsafe evidence deletion reference")
	}
	if d != nil {
		if r.BundleName != "" {
			// Refuse to delete a replacement file that cannot be tied to the intent.
			if _, _, err := d.validatePublished(r); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("refusing evidence deletion: %w", err)
			}
			if err := d.remove(final); err != nil {
				return err
			}
		}
		if err := d.remove(temp); err != nil {
			return err
		}
	}
	r.Deleting = false
	r.DeletedAt = at.UTC()
	return store.UpdateEvidence(ctx, path, r)
}

// InspectFile checks an already published bundle without creating directories,
// reconciling metadata, deleting files, calling Docker, or returning log text.
func InspectFile(path string, r store.CaptureRecord) (string, error) {
	if r.BundleName == "" {
		return "", nil
	}
	final, _ := names(r.ActionID)
	if r.BundleName != final {
		return "", errors.New("invalid evidence bundle name")
	}
	dir, err := directoryPath(path)
	if err != nil {
		return "", err
	}
	location := filepath.Join(dir, final)
	if !r.DeletedAt.IsZero() {
		return location, nil
	}
	d, err := openDirectory(path, false)
	if err != nil {
		return location, err
	}
	defer d.root.Close()
	_, _, err = d.validatePublished(r)
	return location, err
}
