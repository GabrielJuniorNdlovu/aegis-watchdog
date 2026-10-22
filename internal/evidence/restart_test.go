package evidence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

func TestRestartSendsOneBoundedExactIDRequest(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var restarts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
			w.Header().Set("API-Version", "1.52")
			return
		}
		restarts.Add(1)
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/containers/"+id+"/restart") || r.URL.Query().Get("t") != "10" {
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	docker, err := NewDockerClient(client.WithHost(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), RequestTestDeadline)
	defer cancel()
	if err := docker.Restart(ctx, id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restart sent %d mutation requests", restarts.Load())
	}
}

func TestRestartCancellationIsBounded(t *testing.T) {
	const id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var restarts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
			w.Header().Set("API-Version", "1.52")
			return
		}
		restarts.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	docker, err := NewDockerClient(client.WithHost(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = docker.Restart(ctx, id, 10*time.Second)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected canceled request, err=%v", err)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restart sent %d mutation requests", restarts.Load())
	}
}

const RequestTestDeadline = 2 * time.Second
