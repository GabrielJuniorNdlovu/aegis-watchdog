package inspection

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvidenceAllowlistAndMissingExitCode(t *testing.T) {
	raw := json.RawMessage(`{"Config":{"Env":["SECRET"]},"State":{"StartedAt":"2026-10-21T12:00:00Z","Health":{"Log":[{"Start":"2026-10-21T12:00:01Z","End":"2026-10-21T12:00:02Z","ExitCode":1,"Output":"SECRET"},{"Start":"2026-10-21T12:00:03Z","End":"2026-10-21T12:00:04Z"}]}}}`)
	e, err := readEvidence(raw, true)
	if err != nil || len(e.Checks) != 2 || *e.Checks[0].ExitCode != 1 || e.Checks[1].ExitCode != nil {
		t.Fatalf("%+v %v", e, err)
	}
	saved, err := json.Marshal(e)
	if err != nil || strings.Contains(string(saved), "SECRET") {
		t.Fatal("unsafe evidence", err)
	}
}
