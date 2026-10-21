package inspection

import (
	"encoding/json"
	"time"
)

// Check excludes probe output and commands. A nil exit code is not success.
type Check struct {
	Start    time.Time
	End      time.Time
	ExitCode *int
}

type Evidence struct {
	StartedAt  time.Time
	Configured bool
	Checks     []Check
}

// Decode only the fields whose presence the SDK's scalar types cannot preserve.
func readEvidence(raw json.RawMessage, configured bool) (Evidence, error) {
	var fields struct {
		State struct {
			StartedAt string
			Health    *struct{ Log []Check }
		}
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Evidence{}, err
	}
	evidence := Evidence{Configured: configured}
	if fields.State.StartedAt != "" {
		var err error
		evidence.StartedAt, err = time.Parse(time.RFC3339Nano, fields.State.StartedAt)
		if err != nil {
			return Evidence{}, err
		}
	}
	if configured && fields.State.Health != nil {
		evidence.Checks = fields.State.Health.Log
	}
	return evidence, nil
}
