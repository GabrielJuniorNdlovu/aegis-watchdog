package inspection

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

const testID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const secret = "DO_NOT_PRINT_SECRET"

func fixture(health string) string {
	return fmt.Sprintf(`{"Id":%q,"Name":"/demo","Config":{"Env":["TOKEN=%s"],"Healthcheck":{"Test":["CMD-SHELL","secret command"]}},"State":{"Status":"running","Health":%s}}`, testID, secret, health)
}

func TestInspect(t *testing.T) {
	for _, tc := range []struct {
		name, body, health, timestamp, explanation string
		status                                     int
		wantErr                                    string
	}{
		{name: "healthy", body: fixture(`{"Status":"healthy","Log":[{"End":"2026-10-19T10:00:00Z","Output":"` + secret + `"}]}`), health: "healthy", timestamp: "2026-10-19T10:00:00Z", explanation: "passing"},
		{name: "unhealthy", body: fixture(`{"Status":"unhealthy"}`), health: "unhealthy", explanation: "failed"},
		{name: "starting", body: fixture(`{"Status":"starting","Log":[]}`), health: "starting", explanation: "starting"},
		{name: "empty history", body: fixture(`{"Status":"healthy","Log":[]}`), health: "healthy", explanation: "No completed"},
		{name: "null health", body: fixture(`null`), health: "unknown", explanation: "not supplied"},
		{name: "no healthcheck", body: strings.Replace(fixture(`null`), `,"Healthcheck":{"Test":["CMD-SHELL","secret command"]}`, "", 1), health: "unknown", explanation: "No Docker health check"},
		{name: "disabled healthcheck", body: strings.Replace(fixture(`{"Status":"healthy"}`), `["CMD-SHELL","secret command"]`, `["NONE"]`, 1), health: "unknown", explanation: "No Docker health check"},
		{name: "empty healthcheck", body: strings.Replace(fixture(`null`), `["CMD-SHELL","secret command"]`, `[]`, 1), health: "unknown", explanation: "No Docker health check"},
		{name: "incomplete history", body: fixture(`{"Status":"starting","Log":[null,{}, {"Start":"2026-10-19T10:00:00Z"}]}`), health: "starting", explanation: "No completed"},
		{name: "latest completed", body: fixture(`{"Status":"healthy","Log":[{"End":"2026-10-19T10:00:00Z"},{"End":"2026-10-19T09:00:00Z"},{}]}`), health: "healthy", timestamp: "2026-10-19T10:00:00Z"},
		{name: "stopped lifecycle", body: strings.Replace(fixture(`{"Status":"healthy"}`), `"running"`, `"exited"`, 1), health: "healthy", explanation: "retained health data"},
		{name: "not found", status: 404, body: `{"message":"` + secret + `"}`, wantErr: "not found"},
		{name: "access denied", status: 403, body: `{"message":"` + secret + `"}`, wantErr: "access denied"},
		{name: "unauthenticated", status: 401, body: `{"message":"` + secret + `"}`, wantErr: "access denied"},
		{name: "daemon error", status: 500, body: `{"message":"` + secret + `"}`, wantErr: "inspection failed"},
		{name: "malformed JSON", body: `{"` + secret, wantErr: "inspection failed"},
		{name: "null response", body: `null`, wantErr: "invalid or incomplete"},
		{name: "empty response", body: `{}`, wantErr: "invalid or incomplete"},
		{name: "missing ID", body: strings.Replace(fixture(`null`), testID, "", 1), wantErr: "invalid or incomplete"},
		{name: "invalid name", body: strings.Replace(fixture(`null`), "/demo", "bad name", 1), wantErr: "invalid or incomplete"},
		{name: "missing state", body: fmt.Sprintf(`{"Id":%q,"Name":"/demo","Config":{}}`, testID), wantErr: "invalid or incomplete"},
		{name: "missing config", body: fmt.Sprintf(`{"Id":%q,"Name":"/demo","State":{"Status":"running"}}`, testID), wantErr: "invalid or incomplete"},
		{name: "missing lifecycle", body: strings.Replace(fixture(`null`), `"running"`, `""`, 1), wantErr: "invalid or incomplete"},
		{name: "invalid health", body: fixture(`{"Status":"bogus"}`), wantErr: "invalid or incomplete"},
		{name: "missing health status", body: fixture(`{}`), wantErr: "invalid or incomplete"},
		{name: "invalid timestamp", body: fixture(`{"Status":"healthy","Log":[{"End":"bad"}]}`), wantErr: "inspection failed"},
		{name: "reversed timestamp", body: fixture(`{"Status":"healthy","Log":[{"Start":"2026-10-19T10:00:00Z","End":"2026-10-19T09:00:00Z"}]}`), wantErr: "invalid or incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				mu.Unlock()
				if r.Method == "HEAD" && r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.52")
					return
				}
				if r.Method != "GET" || r.URL.RequestURI() != "/v1.52/containers/demo/json" {
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.RequestURI())
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			docker, err := client.New(client.WithHost(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := Inspect(ctx, docker, "demo")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatal("secret leaked in error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.ID != testID || result.Name != "demo" || result.Health != tc.health {
					t.Fatalf("unexpected result: %+v", result)
				}
				if !strings.Contains(result.Explanation, tc.explanation) {
					t.Fatalf("explanation = %q", result.Explanation)
				}
				if tc.timestamp == "" {
					if !result.CheckTime.IsZero() {
						t.Fatal("unexpected timestamp")
					}
				} else if result.CheckTime.Format(time.RFC3339) != tc.timestamp {
					t.Fatalf("timestamp = %v", result.CheckTime)
				}
				if tc.name == "stopped lifecycle" && result.Lifecycle != "exited" {
					t.Fatal("lifecycle lost")
				}
				output := result.Text(time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC))
				if strings.Contains(output, secret) || strings.Contains(output, "secret command") {
					t.Fatal("secret leaked in output")
				}
				if !strings.Contains(output, "Action: none — inspection only.") {
					t.Fatal("missing action")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"HEAD /_ping", "GET /v1.52/containers/demo/json"}; !reflect.DeepEqual(requests, want) {
				t.Fatalf("requests = %v, want %v", requests, want)
			}
		})
	}
}

func TestDeadlineAndCancellation(t *testing.T) {
	for _, phase := range []string{"negotiation", "inspection"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%v", phase, canceled), func(t *testing.T) {
				entered := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/_ping" && phase == "inspection" {
						w.Header().Set("API-Version", "1.52")
						return
					}
					select {
					case <-entered:
					default:
						close(entered)
					}
					<-r.Context().Done()
				}))
				defer server.Close()
				docker, err := client.New(client.WithHost(server.URL))
				if err != nil {
					t.Fatal(err)
				}
				defer docker.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				if canceled {
					go func() {
						select {
						case <-entered:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				_, err = Inspect(ctx, docker, "demo")
				want := "timed out"
				if canceled {
					want = "canceled"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %s", err, want)
				}
			})
		}
	}
}

func TestUnavailableAndSocketPermissions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialErr error
		want    string
	}{
		{"unavailable", syscall.ECONNREFUSED, "unavailable"},
		{"permission", syscall.EACCES, "access denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, tc.dialErr }}
			docker, err := client.New(client.WithHost("http://localhost"), client.WithHTTPClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			_, err = Inspect(context.Background(), docker, "demo")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTextAge(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, "unavailable"},
		{now.Add(-30 * time.Second), "2026-10-19T10:00:00Z (age: 30s)"},
		{now.Add(time.Minute), "age: unknown (check timestamp is in the future"},
	} {
		if output := (Result{CheckTime: tc.at}).Text(now); !strings.Contains(output, tc.want) {
			t.Fatalf("output = %q, want %q", output, tc.want)
		}
	}
}

func TestSelectorAndNegotiation(t *testing.T) {
	for _, selector := range []string{"demo", testID, testID[:12], "/demo"} {
		t.Run(selector, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				mu.Unlock()
				if r.URL.Path == "/_ping" {
					if r.Method == "HEAD" {
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					if r.Method == "GET" {
						w.Header().Set("API-Version", "1.52")
						return
					}
				}
				if r.Method != "GET" || r.URL.Path != "/v1.52/containers/"+strings.TrimPrefix(selector, "/")+"/json" || r.URL.RawQuery != "" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, fixture(`{"Status":"healthy"}`))
			}))
			defer server.Close()
			docker, err := client.New(client.WithHost(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			if _, err := Inspect(context.Background(), docker, selector); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"HEAD /_ping", "GET /_ping", "GET /v1.52/containers/" + strings.TrimPrefix(selector, "/") + "/json"}
			if !reflect.DeepEqual(requests, want) {
				t.Fatalf("requests=%v, want %v", requests, want)
			}
		})
	}
}
