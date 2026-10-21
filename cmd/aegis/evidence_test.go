package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/evidence"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestEvidenceInspectionIsReadOnlyWithoutDocker(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "line\nbreak", "aegis.db")
	result, err := store.Reserve(ctx, path, store.Proposal{ServiceID: "demo", ContainerID: strings.Repeat("a", 64), Policy: budget.DefaultPolicy()}, at)
	if err != nil || result.Reservation == nil {
		t.Fatal(result, err)
	}
	in, _ := json.Marshal(evidence.Input{ActionID: 1, ServiceID: "demo", ContainerID: strings.Repeat("a", 64), Policy: budget.DefaultPolicy()})
	limits, _ := json.Marshal(evidence.DefaultLimits())
	r := store.CaptureRecord{ActionID: 1, Status: "pending", StartedAt: at, Input: string(in), Limits: string(limits)}
	if err = store.BeginEvidence(ctx, path, r); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "partial", "failed", "complete"} {
		t.Run(status, func(t *testing.T) {
			r.Status = status
			r.EndedAt = at
			r.BundleName = "capture-1.json"
			r.Bytes = 100
			r.Truncated = true
			r.Issues = []string{"missing\nline\x1b[31m"}
			if err = store.UpdateEvidence(ctx, path, r); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := run(ctx, []string{"actions", "--id", "1", "--db", path}, &out, &errOut); code != 0 || errOut.Len() != 0 {
				t.Fatal(code, errOut.String())
			}
			for _, part := range []string{"Evidence status: " + status, "Capture started at:", "Published bytes: 100", "Evidence truncated: true", "Evidence file check: unavailable or invalid", `line\nbreak`, `missing\nline\x1b[31m`} {
				if !strings.Contains(out.String(), part) {
					t.Fatal(part, out.String())
				}
			}
			if strings.Contains(out.String(), "line\nbreak") || strings.Contains(out.String(), "\x1b") {
				t.Fatal("unescaped control characters")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("inspection changed database", err)
			}
			if _, err = os.Lstat(filepath.Join(filepath.Dir(path), "evidence")); !os.IsNotExist(err) {
				t.Fatal("inspection created evidence directory", err)
			}
		})
	}
}
