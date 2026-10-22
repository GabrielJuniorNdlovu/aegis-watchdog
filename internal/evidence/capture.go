package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/ownership"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

// Capture is internal: callers must already have a reservation. There is no
// public command to manufacture actions, and the live monitor never calls it.
// A repeated request returns the original record without collecting again.
func Capture(ctx context.Context, path string, id int64, docker *DockerClient, p Limits, now func() time.Time) (store.CaptureRecord, error) {
	lock, err := ownership.Acquire(path)
	if err != nil {
		return store.CaptureRecord{}, err
	}
	defer lock.Close()
	return captureLocked(ctx, path, id, docker, p, now)
}

// CaptureLocked is for the monitor, which already holds the data-directory lock.
func CaptureLocked(ctx context.Context, path string, id int64, docker *DockerClient, p Limits, now func() time.Time) (store.CaptureRecord, error) {
	return captureLocked(ctx, path, id, docker, p, now)
}

func captureLocked(ctx context.Context, path string, id int64, docker *DockerClient, p Limits, now func() time.Time) (store.CaptureRecord, error) {
	var r store.CaptureRecord
	if id <= 0 {
		return r, errors.New("evidence capture requires a positive action ID")
	}
	if err := p.Validate(); err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Deadline)
	defer cancel()
	var err error
	if err = ReconcileLocked(ctx, path, now()); err != nil {
		return r, err
	}
	existing, err := store.EvidenceRecords(ctx, path, id, 1)
	if err != nil {
		return r, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	active, err := store.EvidenceRecords(ctx, path, 0, MaxEntries+1)
	if err != nil {
		return r, err
	}
	if len(active) >= MaxEntries {
		return r, errors.New("evidence registry entry limit reached; resolve and retire old captures first")
	}
	reservation, assessments, missing, err := store.EvidenceInput(ctx, path, id)
	if err != nil {
		return r, err
	}
	input := Input{ActionID: id, ServiceID: reservation.ServiceID, ContainerID: reservation.ContainerID, ReservedAt: reservation.ReservedAt, Policy: reservation.Policy, MissingAssessments: missing}
	for _, a := range assessments {
		input.Assessments = append(input.Assessments, Assessment{a.EvaluatedAt, a.Status, a.Reason, a.Explanation, a.Policy, a.EvidenceEnd})
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return r, err
	}
	limits, err := json.Marshal(p)
	if err != nil {
		return r, err
	}
	r = store.CaptureRecord{ActionID: id, Status: "pending", StartedAt: now().UTC(), Input: string(encoded), Limits: string(limits)}
	if err = store.BeginEvidence(ctx, path, r); err != nil {
		return r, fmt.Errorf("record evidence intent: %w", err)
	}
	fail := func(code string, cause error) (store.CaptureRecord, error) {
		r.Status = "failed"
		r.EndedAt = now().UTC()
		r.Issues = []string{code}
		if recordErr := recordResult(path, r); recordErr != nil {
			return r, fmt.Errorf("%s; cannot record evidence failure: %w", code, recordErr)
		}
		return r, fmt.Errorf("%s: %w", code, cause)
	}
	d, err := openDirectory(path, true)
	if err != nil {
		return fail("evidence_directory_unavailable", err)
	}
	defer d.root.Close()
	used, err := d.usage()
	if err != nil {
		return fail("evidence_storage_unavailable", err)
	}
	if used > p.TotalBytes-p.BundleBytes {
		return fail("evidence_storage_allowance_exhausted", errors.New("protected and unknown files are never removed to admit a capture"))
	}
	// Keep the intent's start time stable across both persistence domains.
	first := true
	b, err := Collect(ctx, docker, input, p, func() time.Time {
		if first {
			first = false
			return r.StartedAt
		}
		return now()
	})
	if err != nil {
		return fail("evidence_collection_failed", err)
	}
	body, err := json.Marshal(b)
	if err != nil {
		return fail("evidence_encoding_failed", err)
	}
	if int64(len(body)) > p.BundleBytes {
		return fail("evidence_bundle_allowance_exceeded", errors.New("encoded bundle exceeds allowance"))
	}
	if err = d.publish(id, body); err != nil {
		// A final name may already be durable. Leave pending for validation at startup.
		final, _ := names(id)
		if _, statErr := d.root.Lstat(final); statErr == nil {
			return r, fmt.Errorf("evidence publication needs reconciliation: %w", err)
		}
		return fail("evidence_publication_failed", err)
	}
	r = publishedRecord(r, b, body)
	if err = recordResult(path, r); err != nil {
		return r, fmt.Errorf("bundle published but SQLite completion failed: %w", err)
	}
	return r, nil
}

// Recording after cancellation gets its own small deadline. Never report success
// when this final durable step fails.
func recordResult(path string, r store.CaptureRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return store.UpdateEvidence(ctx, path, r)
}
func publishedRecord(r store.CaptureRecord, b Bundle, body []byte) store.CaptureRecord {
	r.Status = b.Status
	r.EndedAt = b.EndedAt
	r.BundleName, _ = names(r.ActionID)
	r.Bytes = int64(len(body))
	r.Digest = digest(body)
	r.Truncated = b.Truncated
	r.Issues = b.Issues
	return r
}

// ReconcileLocked runs under the existing data-directory owner lock. It has no
// Docker dependency and never modifies reservations or action audit history.
func ReconcileLocked(ctx context.Context, path string, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultLimits().Deadline)
	defer cancel()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	records, err := store.EvidenceRecords(ctx, path, 0, MaxEntries+1)
	if err != nil {
		return err
	}
	if len(records) > MaxEntries {
		return errors.New("evidence registry entry limit reached")
	}
	if len(records) == 0 {
		return nil
	}
	d, openErr := openDirectory(path, false)
	if openErr != nil && !errors.Is(openErr, os.ErrNotExist) {
		return fmt.Errorf("reconcile evidence directory: %w", openErr)
	}
	if d != nil {
		defer d.root.Close()
	}
	for _, r := range records {
		if err = ctx.Err(); err != nil {
			return err
		}
		if r.Deleting {
			if err = finishDeletion(ctx, path, d, r, at); err != nil {
				return err
			}
			continue
		}
		if r.Status != "pending" && r.BundleName == "" {
			continue
		}
		var b Bundle
		var body []byte
		validationErr := openErr
		if d != nil {
			b, body, validationErr = d.validatePublished(r)
		}
		if validationErr == nil {
			if r.Status == "pending" {
				if err = d.sync(); err != nil {
					return err
				}
				r = publishedRecord(r, b, body)
				if err = store.UpdateEvidence(ctx, path, r); err != nil {
					return err
				}
			}
		} else {
			code := "published_evidence_missing_or_invalid"
			if r.Status == "pending" {
				code = "capture_interrupted_before_valid_publication"
			}
			// Preserve published references for diagnosis, but never call them complete.
			if r.Status != "failed" || !contains(r.Issues, code) {
				r.Status = "failed"
				r.EndedAt = at.UTC()
				r.Issues = append(r.Issues, code)
				if err = store.UpdateEvidence(ctx, path, r); err != nil {
					return err
				}
			}
		}
		if d != nil {
			_, temp := names(r.ActionID)
			if err = d.remove(temp); err != nil {
				return fmt.Errorf("remove abandoned evidence temporary file: %w", err)
			}
		}
	}
	return nil
}
func contains(values []string, value string) bool {
	for _, s := range values {
		if s == value {
			return true
		}
	}
	return false
}
