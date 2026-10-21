package evidence

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/assessment"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/moby/moby/client"
)

// This opt-in test receives only the full ID of the script's isolated sandbox.
// All reservation records live in a new, disposable directory.
func TestLiveEvidenceDemo(t *testing.T) {
	target := os.Getenv("AEGIS_EVIDENCE_DEMO_CONTAINER")
	if target == "" {
		t.Skip("run bash scripts/smoke-evidence.sh for the isolated Docker demo")
	}
	if !inspection.ValidID(target) {
		t.Fatal("demo requires a full container ID")
	}
	root := os.Getenv("AEGIS_EVIDENCE_DEMO_DIR")
	if root == "" {
		root = t.TempDir()
	} else {
		info, err := os.Lstat(root)
		if err != nil || info.Mode() != os.ModeDir|0700 {
			t.Fatal("demo requires a real private directory", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("demo directory must be empty", err)
		}
	}
	path := filepath.Join(root, "simulated-actions.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	docker, err := NewDockerClient(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	before, err := inspection.Inspect(ctx, docker.sdk, target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(before.Name, "aegis-evidence-") {
		t.Fatal("demo target must belong to the isolated evidence Compose project")
	}
	at := time.Now().UTC()
	a := assessment.Assessment{ContainerID: target, EvaluatedAt: at, CollectedAt: at, Status: "unknown", Reason: "simulated_demo", Explanation: "Simulated reservation for isolated evidence capture; no recovery operation.", Policy: assessment.DefaultPolicy()}
	if err = store.SaveMonitoringResult(ctx, path, before.Name, before, nil, a); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.Reserve(ctx, path, store.Proposal{ServiceID: "evidence-demo", ContainerID: target, Policy: budget.DefaultPolicy(), AssessmentID: 1}, at)
	if err != nil || reserved.Reservation == nil {
		t.Fatal(reserved, err)
	}
	budgetBefore, err := store.InspectBudget(ctx, path, "evidence-demo", budget.DefaultPolicy(), at, 20)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Capture(ctx, path, reserved.Reservation.ID, docker, DefaultLimits(), time.Now)
	if err != nil || r.Status != "complete" {
		t.Fatal(r, err)
	}
	location, err := InspectFile(path, r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		t.Fatal(err)
	}
	var b Bundle
	if err = json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Logs, "synthetic evidence demo") || !strings.Contains(b.Logs, "[REDACTED]") {
		t.Fatal("synthetic logs or redaction missing")
	}
	for _, secret := range []string{"DEMO_AUTH_SECRET", "DEMO_TOKEN_SECRET", "DEMO_PASSWORD_SECRET", "DEMO_ENV_SECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("synthetic secret persisted", secret)
		}
	}
	budgetAfter, err := store.InspectBudget(ctx, path, "evidence-demo", budget.DefaultPolicy(), at, 20)
	if err != nil || !reflect.DeepEqual(budgetBefore, budgetAfter) {
		t.Fatal("action or budget changed", err)
	}
	again, err := Capture(ctx, path, reserved.Reservation.ID, docker, DefaultLimits(), time.Now)
	if err != nil || again.Digest != r.Digest {
		t.Fatal("repeat overwrote capture", err)
	}
	t.Log("PASS: SIMULATED reservation; exact sandbox container; bounded redacted bundle; unchanged action and budget; repeat preserved")
	t.Logf("SIMULATED database: %s", path)
	t.Logf("Bundle: %s (%d bytes)", location, r.Bytes)
}
