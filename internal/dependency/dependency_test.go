package dependency

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/moby/moby/client"
)

func TestLoadAndTargetValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "services.json")
	if err := os.WriteFile(path, []byte(`{"services":[{"id":"api","container":"api","dependencies":["db"]},{"id":"db","container":"database"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Load(path)
	if err != nil || manifest.ValidateTarget("api", "/api") != nil {
		t.Fatalf("valid service graph rejected: %#v %v", manifest, err)
	}
	if err := manifest.ValidateTarget("missing", "api"); err == nil {
		t.Fatal("unknown target service accepted")
	}
	if err := manifest.ValidateTarget("api", "other"); err == nil {
		t.Fatal("target selector mismatch accepted")
	}
	if err := os.WriteFile(path, []byte(`{"services":[],"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid or unknown service configuration accepted")
	}
}

func TestManifestValidationRejectsInvalidGraphs(t *testing.T) {
	for name, manifest := range map[string]Manifest{
		"unknown":              {Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"database"}}}},
		"self":                 {Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"api"}}}},
		"cycle":                {Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"db"}}, {ID: "db", Container: "db", Dependencies: []string{"api"}}}},
		"duplicate":            {Services: []Service{{ID: "api", Container: "api"}, {ID: "api", Container: "other"}}},
		"duplicate dependency": {Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"db", "db"}}, {ID: "db", Container: "db"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := manifest.Validate(); err == nil {
				t.Fatal("invalid service graph was accepted")
			}
		})
	}
}

func TestEvaluateDirectDependenciesFreshnessAndStableBlockerOrder(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Services: []Service{
		{ID: "api", Container: "api", Dependencies: []string{"zeta", "alpha"}},
		{ID: "alpha", Container: "alpha"},
		{ID: "zeta", Container: "zeta", Dependencies: []string{"unknown-transit"}},
		{ID: "unknown-transit", Container: "transit"},
	}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	observations := map[string]Observation{
		"alpha": {ServiceID: "alpha", ContainerID: strings.Repeat("a", 64), Execution: at.Add(-time.Hour), ObservedAt: at, CheckTime: at.Add(-time.Second), Lifecycle: "running", Health: "unhealthy", HealthCheck: true},
		"zeta":  {ServiceID: "zeta", ContainerID: strings.Repeat("b", 64), Execution: at.Add(-time.Hour), ObservedAt: at, CheckTime: at.Add(-time.Second), Lifecycle: "paused", Health: "healthy", HealthCheck: true},
	}
	report := Evaluate(manifest, "api", observations, nil, at, time.Minute)
	if report.Passed || len(report.Blocked()) != 2 || report.Observations[0].ServiceID != "alpha" || report.Observations[1].ServiceID != "zeta" {
		t.Fatalf("unstable or incomplete blockers: %#v", report)
	}
	if report.Observations[0].ReasonCode != "health_unhealthy" || report.Observations[1].ReasonCode != "lifecycle_paused" {
		t.Fatalf("wrong reasons: %#v", report.Observations)
	}
	if strings.Contains(report.Summary(), "unknown-transit") {
		t.Fatalf("gate recursively evaluated dependency: %s", report.Summary())
	}
}

func TestEvaluateBlocksUnknownMissingStaleHealthAndChangedExecution(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"db"}}, {ID: "db", Container: "database"}}}
	id := strings.Repeat("d", 64)
	base := Observation{ServiceID: "db", Selector: "database", ContainerID: id, Execution: at.Add(-time.Hour), ObservedAt: at, CheckTime: at.Add(-time.Second), Lifecycle: "running", Health: "healthy", HealthCheck: true}
	checks := []struct {
		name string
		obs  map[string]Observation
		want string
	}{
		{"missing", nil, "missing_observation"},
		{"stale underlying health despite recent inspect", map[string]Observation{"db": func() Observation { o := base; o.CheckTime = at.Add(-2 * time.Minute); return o }()}, "health_stale"},
		{"no health check", map[string]Observation{"db": func() Observation { o := base; o.HealthCheck = false; return o }()}, "health_check_missing"},
		{"missing check timestamp", map[string]Observation{"db": func() Observation { o := base; o.CheckTime = time.Time{}; return o }()}, "health_timestamp_missing"},
		{"unknown", map[string]Observation{"db": func() Observation { o := base; o.Health = "unknown"; return o }()}, "health_unknown"},
		{"stopped", map[string]Observation{"db": func() Observation { o := base; o.Lifecycle = "exited"; return o }()}, "lifecycle_exited"},
		{"collection failure", map[string]Observation{"db": {ServiceID: "db", Selector: "database", ErrorCategory: "unavailable", ObservedAt: at}}, "collection_error"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			report := Evaluate(manifest, "api", tc.obs, nil, at, time.Minute)
			if report.Passed || len(report.Observations) != 1 || report.Observations[0].ReasonCode != tc.want {
				t.Fatalf("got %#v, want %s", report, tc.want)
			}
		})
	}
	previous := Evaluate(manifest, "api", map[string]Observation{"db": base}, nil, at, time.Minute)
	changed := base
	changed.Execution = at.Add(-time.Minute)
	report := Evaluate(manifest, "api", map[string]Observation{"db": changed}, &previous, at, time.Minute)
	if report.Passed || report.Observations[0].ReasonCode != "identity_changed" {
		t.Fatalf("changed execution was accepted: %#v", report)
	}
}

func TestAllDirectDependenciesHealthyPass(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"cache", "database"}}, {ID: "cache", Container: "cache"}, {ID: "database", Container: "db"}}}
	observations := map[string]Observation{}
	for _, id := range []string{"cache", "database"} {
		observations[id] = Observation{ServiceID: id, ContainerID: strings.Repeat("a", 64), Execution: at.Add(-time.Hour), ObservedAt: at, CheckTime: at.Add(-time.Second), Lifecycle: "running", Health: "healthy", HealthCheck: true}
	}
	report := Evaluate(manifest, "api", observations, nil, at, time.Minute)
	if !report.Passed || len(report.Observations) != 2 || report.Observations[0].ServiceID != "cache" || report.Observations[1].ServiceID != "database" {
		t.Fatalf("healthy gates did not pass in stable order: %#v", report)
	}
}

type fakeInspector struct {
	calls   []string
	results map[string]inspection.Result
}

type sdkInspector struct{ docker *client.Client }

type dockerRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip dockerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func (s sdkInspector) Inspect(ctx context.Context, selector string) (inspection.Result, error) {
	return inspection.Inspect(ctx, s.docker, selector)
}

func (f *fakeInspector) Inspect(_ context.Context, selector string) (inspection.Result, error) {
	f.calls = append(f.calls, selector)
	return f.results[selector], nil
}

func TestCollectDeduplicatesSharedSelectorsAndReusesTargetObservation(t *testing.T) {
	at := time.Now().UTC()
	manifest := Manifest{Services: []Service{
		{ID: "api", Container: "api", Dependencies: []string{"db-a", "db-b", "self"}},
		{ID: "db-a", Container: "database"}, {ID: "db-b", Container: "database"}, {ID: "self", Container: "api"},
	}}
	result := inspection.Result{ID: strings.Repeat("a", 64), Lifecycle: "running", Health: "healthy", CheckTime: at, Evidence: inspection.Evidence{Configured: true, StartedAt: at}}
	fake := &fakeInspector{results: map[string]inspection.Result{"database": result}}
	target := FromResult("api", "api", result, nil, at)
	observations := Collect(context.Background(), manifest, "api", fake, &target, time.Now)
	if len(fake.calls) != 1 || fake.calls[0] != "database" || len(observations) != 3 {
		t.Fatalf("duplicate or unnecessary inspections: %#v / %#v", fake.calls, observations)
	}
	if observations["self"].ContainerID != target.ContainerID {
		t.Fatal("target observation was not reused")
	}
}

func TestDependencyInspectionUsesOnlyReadOnlyDockerAPIRequests(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	id := strings.Repeat("d", 64)
	var gets, mutations int
	transport := dockerRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/_ping" {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Api-Version": []string{"1.52"}}, Body: http.NoBody, Request: r}, nil
		}
		if r.Method == http.MethodGet && r.URL.RequestURI() == "/v1.52/containers/database/json" {
			gets++
			body := fmt.Sprintf(`{"Id":%q,"Name":"/database","Config":{"Healthcheck":{"Test":["CMD","true"]}},"HostConfig":{"RestartPolicy":{"Name":"no"}},"State":{"Status":"running","StartedAt":"%s","Health":{"Status":"healthy","Log":[{"Start":"%s","End":"%s","ExitCode":0}]}}}`, id, at.Add(-time.Hour).Format(time.RFC3339Nano), at.Add(-2*time.Second).Format(time.RFC3339Nano), at.Add(-time.Second).Format(time.RFC3339Nano))
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mutations++
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unexpected request")), Request: r}, nil
	})
	docker, err := client.New(client.WithHost("tcp://localhost:2375"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	manifest := Manifest{Services: []Service{{ID: "api", Container: "api", Dependencies: []string{"database", "database-copy"}}, {ID: "database", Container: "database"}, {ID: "database-copy", Container: "database"}}}
	observations := Collect(context.Background(), manifest, "api", sdkInspector{docker}, nil, func() time.Time { return at })
	report := Evaluate(manifest, "api", observations, nil, at, time.Minute)
	if !report.Passed || gets != 1 || mutations != 0 || len(observations) != 2 {
		t.Fatalf("dependency gate or Docker request sequence incorrect: gate=%#v gets=%d mutations=%d observations=%#v", report, gets, mutations, observations)
	}
}
