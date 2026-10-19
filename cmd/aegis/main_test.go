package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func dockerEnv(t *testing.T, host string) {
	t.Helper()
	for _, name := range []string{"DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		t.Setenv(name, "")
	}
	t.Setenv("DOCKER_HOST", host)
}

func TestArgumentsBeforeDocker(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	dockerEnv(t, server.URL)
	for _, args := range [][]string{
		nil, {"monitor"}, {"inspect"}, {"inspect", "--container"}, {"inspect", "--container="},
		{"inspect", "--container", " "}, {"inspect", "--container", "../other"},
		{"inspect", "--container", "demo", "extra"}, {"inspect", "--container", "demo", "--unknown"},
		{"inspect", "--container", "demo", "--container", "other"}, {"inspect", "--container", "demo?size=1"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("args %q: code=%d stdout=%q stderr=%q", args, code, out.String(), errOut.String())
		}
	}
	for _, args := range [][]string{{"--help"}, {"inspect", "--help"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 0 || !strings.Contains(out.String(), "Usage:") || errOut.Len() != 0 {
			t.Fatalf("help code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid arguments made %d Docker requests", calls.Load())
	}
}

func TestExitBehavior(t *testing.T) {
	for _, health := range []string{"healthy", "unhealthy", "starting", "unknown", "missing", "invalid"} {
		t.Run(health, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" && r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.52")
					return
				}
				if r.Method != "GET" || r.URL.Path != "/v1.52/containers/demo/json" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if health == "missing" {
					w.WriteHeader(404)
					fmt.Fprint(w, `{"message":"SECRET"}`)
					return
				}
				if health == "invalid" {
					fmt.Fprint(w, `{"Config":{"Env":["SECRET"]}}`)
					return
				}
				config, state := `{}`, `{"Status":"running"}`
				if health != "unknown" {
					config = `{"Healthcheck":{"Test":["CMD","true"]}}`
					state = fmt.Sprintf(`{"Status":"running","Health":{"Status":%q}}`, health)
				}
				fmt.Fprintf(w, `{"Id":"%s","Name":"/demo","Config":%s,"State":%s}`, strings.Repeat("a", 64), config, state)
			}))
			defer server.Close()
			dockerEnv(t, server.URL)
			var out, errOut bytes.Buffer
			code := run(context.Background(), []string{"inspect", "--container", "demo"}, &out, &errOut)
			if health == "missing" || health == "invalid" {
				if code != 1 || out.Len() != 0 || errOut.Len() == 0 || strings.Contains(errOut.String(), "SECRET") {
					t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
				}
			} else if code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "Health: "+health) || !strings.Contains(out.String(), "Action: none — inspection only.") {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestInvalidDockerConfiguration(t *testing.T) {
	dockerEnv(t, "invalid-SECRET-host")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "--container", "demo"}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "configuration") || strings.Contains(errOut.String(), "SECRET") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestCanceledInspectionExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("canceled context must not make a request")
	}))
	defer server.Close()
	dockerEnv(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"inspect", "--container", "demo"}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "canceled") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" || r.URL.Path != "/v1.52/containers/demo/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Id":"%s","Name":"/demo","Config":{},"State":{"Status":"running"}}`, strings.Repeat("a", 64))
	}))
	defer server.Close()
	dockerEnv(t, server.URL)
	var errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "--container", "demo"}, brokenOutput{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "cannot write") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}
