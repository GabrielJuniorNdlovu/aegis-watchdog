package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/evidence"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func appendEvidenceInspection(ctx context.Context, path string, id int64, out *strings.Builder) error {
	records, err := store.EvidenceRecords(ctx, path, id, 1)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(out, "Evidence capture: none recorded")
		return nil
	}
	r := records[0]
	fmt.Fprintf(out, "\nEvidence status: %s\nCapture started at: %s\nCapture ended at: %s\nPublished bytes: %d\nEvidence truncated: %t\n", r.Status, timestamp(r.StartedAt), timestamp(r.EndedAt), r.Bytes, r.Truncated)
	location, fileErr := evidence.InspectFile(path, r)
	if location == "" {
		fmt.Fprintln(out, "Published bundle: none")
	} else {
		fmt.Fprintf(out, "Published bundle: %q\n", location)
	}
	if !r.DeletedAt.IsZero() {
		fmt.Fprintf(out, "Evidence retired at: %s (file removed; publication metadata retained)\n", timestamp(r.DeletedAt))
	} else if r.Deleting {
		fmt.Fprintln(out, "Evidence deletion: pending startup reconciliation")
	}
	if fileErr != nil {
		fmt.Fprintf(out, "Evidence file check: unavailable or invalid: %q\n", fileErr.Error())
	}
	for _, issue := range r.Issues {
		fmt.Fprintf(out, "Evidence issue: %q\n", issue)
	}
	return nil
}
