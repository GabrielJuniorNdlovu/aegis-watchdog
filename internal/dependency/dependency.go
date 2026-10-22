// Package dependency validates direct recovery gates and evaluates saved Docker observations.
package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
)

const (
	MaxServices     = 32
	MaxDependencies = 8
	MaxConfigBytes  = 64 << 10
	MaxReportBytes  = 7 << 10
)

type Service struct {
	ID           string   `json:"id"`
	Container    string   `json:"container"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Manifest struct {
	Services []Service `json:"services"`
}

type Observation struct {
	ServiceID     string    `json:"service_id"`
	Selector      string    `json:"selector"`
	ContainerID   string    `json:"container_id,omitempty"`
	Execution     time.Time `json:"execution_started_at,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	CheckTime     time.Time `json:"health_check_at,omitempty"`
	Lifecycle     string    `json:"lifecycle,omitempty"`
	Health        string    `json:"health,omitempty"`
	HealthCheck   bool      `json:"health_check_configured"`
	ErrorCategory string    `json:"collection_error,omitempty"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	Explanation   string    `json:"explanation,omitempty"`
}

type Report struct {
	ServiceID    string        `json:"service_id"`
	EvaluatedAt  time.Time     `json:"evaluated_at"`
	MaxAge       time.Duration `json:"max_age_ns"`
	Passed       bool          `json:"passed"`
	Observations []Observation `json:"dependencies"`
}

type Inspector interface {
	Inspect(context.Context, string) (inspection.Result, error)
}

func Load(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open service configuration: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read service configuration: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return Manifest{}, errors.New("service configuration exceeds 64 KiB")
	}
	var manifest Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse service configuration: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("service configuration must contain one JSON object")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if len(m.Services) == 0 || len(m.Services) > MaxServices {
		return fmt.Errorf("service configuration must contain 1 to %d services", MaxServices)
	}
	services := make(map[string]Service, len(m.Services))
	for _, s := range m.Services {
		if !budget.ValidService(s.ID) || !inspection.ValidSelector(s.Container) || len(s.Dependencies) > MaxDependencies {
			return fmt.Errorf("invalid service configuration for %q", s.ID)
		}
		if _, exists := services[s.ID]; exists {
			return fmt.Errorf("duplicate service ID %q", s.ID)
		}
		services[s.ID] = s
	}
	for _, s := range m.Services {
		seen := make(map[string]bool, len(s.Dependencies))
		for _, id := range s.Dependencies {
			if _, exists := services[id]; !exists {
				return fmt.Errorf("service %q references unknown dependency %q", s.ID, id)
			}
			if id == s.ID {
				return fmt.Errorf("service %q cannot depend on itself", s.ID)
			}
			if seen[id] {
				return fmt.Errorf("service %q lists dependency %q more than once", s.ID, id)
			}
			seen[id] = true
		}
	}
	state := make(map[string]uint8, len(services))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("service dependency cycle includes %q", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range services[id].Dependencies {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range services {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (m Manifest) Service(id string) (Service, bool) {
	for _, service := range m.Services {
		if service.ID == id {
			return service, true
		}
	}
	return Service{}, false
}

func (m Manifest) ValidateTarget(id, selector string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	s, ok := m.Service(id)
	if !ok {
		return fmt.Errorf("service %q is missing from service configuration", id)
	}
	if strings.TrimPrefix(s.Container, "/") != strings.TrimPrefix(selector, "/") {
		return fmt.Errorf("service %q container %q does not match --container %q", id, s.Container, selector)
	}
	return nil
}

func Collect(ctx context.Context, m Manifest, target string, inspector Inspector, targetObservation *Observation, now func() time.Time) map[string]Observation {
	service, ok := m.Service(target)
	if !ok {
		return nil
	}
	deps := append([]string(nil), service.Dependencies...)
	sort.Strings(deps)
	bySelector := make(map[string]Observation, len(deps))
	out := make(map[string]Observation, len(deps))
	for _, id := range deps {
		d, _ := m.Service(id)
		observation, exists := bySelector[d.Container]
		if !exists && targetObservation != nil && d.Container == service.Container {
			observation, exists = *targetObservation, true
		}
		if !exists {
			requestCtx, cancel := context.WithTimeout(ctx, inspection.Timeout)
			result, err := inspector.Inspect(requestCtx, d.Container)
			cancel()
			observation = fromResult(id, d.Container, result, err, now().UTC())
			bySelector[d.Container] = observation
		}
		observation.ServiceID = id
		out[id] = observation
	}
	return out
}

func FromResult(id, selector string, result inspection.Result, err error, observedAt time.Time) Observation {
	return fromResult(id, selector, result, err, observedAt)
}

func fromResult(id, selector string, r inspection.Result, err error, at time.Time) Observation {
	o := Observation{ServiceID: id, Selector: selector, ObservedAt: at.UTC()}
	if err != nil {
		o.ErrorCategory, _ = inspection.FailureDetails(err)
		return o
	}
	o.ContainerID, o.Execution, o.CheckTime = r.ID, r.Evidence.StartedAt, r.CheckTime
	o.Lifecycle, o.Health, o.HealthCheck = r.Lifecycle, r.Health, r.Evidence.Configured
	return o
}

func Evaluate(m Manifest, target string, observations map[string]Observation, previous *Report, at time.Time, maxAge time.Duration) Report {
	report := Report{ServiceID: target, EvaluatedAt: at.UTC(), MaxAge: maxAge, Passed: true, Observations: make([]Observation, 0)}
	service, ok := m.Service(target)
	if !ok {
		report.Passed = false
		return report
	}
	deps := append([]string(nil), service.Dependencies...)
	sort.Strings(deps)
	previousByID := map[string]Observation{}
	if previous != nil {
		for _, observation := range previous.Observations {
			previousByID[observation.ServiceID] = observation
		}
	}
	for _, id := range deps {
		observation, exists := observations[id]
		if !exists {
			observation = Observation{ServiceID: id, Selector: m.serviceContainer(id), ObservedAt: at.UTC()}
			block(&observation, "missing_observation", "no dependency observation was collected")
		} else if observation.ErrorCategory != "" {
			block(&observation, "collection_error", "dependency inspection failed ("+observation.ErrorCategory+")")
		} else if !inspection.ValidID(observation.ContainerID) || observation.Execution.IsZero() {
			block(&observation, "identity_unavailable", "dependency container or execution identity is unavailable")
		} else if p, exists := previousByID[id]; exists && (!sameIdentity(p, observation)) {
			block(&observation, "identity_changed", "dependency container or execution changed since the earlier gate check; fresh monitoring evidence is required")
		} else if observation.Lifecycle != "running" {
			block(&observation, "lifecycle_"+safeCode(observation.Lifecycle), "dependency lifecycle does not permit serving")
		} else if !observation.HealthCheck {
			block(&observation, "health_check_missing", "dependency has no configured Docker health check")
		} else if observation.Health == "unknown" || observation.Health == "starting" {
			block(&observation, "health_unknown", "dependency health is unknown or still starting")
		} else if observation.Health != "healthy" {
			block(&observation, "health_unhealthy", "dependency Docker health is not healthy")
		} else if observation.CheckTime.IsZero() {
			block(&observation, "health_timestamp_missing", "dependency has no completed health-check timestamp")
		} else if observation.CheckTime.After(at) {
			block(&observation, "health_timestamp_future", "dependency health-check timestamp is in the future")
		} else if maxAge <= 0 || at.Sub(observation.CheckTime) > maxAge {
			block(&observation, "health_stale", "dependency's last completed health check is stale")
		}
		report.Observations = append(report.Observations, observation)
		if observation.ReasonCode != "" {
			report.Passed = false
		}
	}
	return report
}

func (m Manifest) serviceContainer(id string) string {
	s, ok := m.Service(id)
	if !ok {
		return ""
	}
	return s.Container
}

func sameIdentity(a, b Observation) bool {
	return a.ContainerID == b.ContainerID && a.Execution.Equal(b.Execution)
}

func block(o *Observation, code, explanation string) {
	o.ReasonCode, o.Explanation = code, explanation
}

func safeCode(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.ToLower(strings.ReplaceAll(s, "-", "_"))
}

func (r Report) Summary() string {
	status := "passed"
	if !r.Passed {
		status = "blocked"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Dependency gate %s for service %s at %s (max age %s).", status, r.ServiceID, r.EvaluatedAt.UTC().Format(time.RFC3339Nano), r.MaxAge)
	for _, observation := range r.Observations {
		fmt.Fprintf(&b, " %s=%s", observation.ServiceID, observation.Health)
		if observation.ReasonCode != "" {
			fmt.Fprintf(&b, " [%s: %s]", observation.ReasonCode, observation.Explanation)
		}
		fmt.Fprintf(&b, " (container=%s execution=%s check=%s observed=%s)", observation.ContainerID, timeText(observation.Execution), timeText(observation.CheckTime), timeText(observation.ObservedAt))
	}
	text := b.String()
	if len(text) > MaxReportBytes {
		text = text[:MaxReportBytes-3] + "..."
	}
	return text
}

func (r Report) Blocked() []Observation {
	var blocked []Observation
	for _, observation := range r.Observations {
		if observation.ReasonCode != "" {
			blocked = append(blocked, observation)
		}
	}
	return blocked
}

func timeText(at time.Time) string {
	if at.IsZero() {
		return "unavailable"
	}
	return at.UTC().Format(time.RFC3339Nano)
}
