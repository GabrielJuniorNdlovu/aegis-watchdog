package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
	"github.com/moby/moby/client"
)

var at = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
var id = strings.Repeat("a", 64)

func config(t *testing.T) Config {
	t.Helper()
	return Config{Target: "demo", DBPath: filepath.Join(t.TempDir(), "history.db"), Interval: 5 * time.Second}
}

func observation() inspection.Result {
	return inspection.Result{ID: id, Name: "demo", Lifecycle: "running", Health: "healthy", CheckTime: at.Add(-time.Minute)}
}

func TestImmediateCollectionAndCompletionScheduling(t *testing.T) {
	cfg := config(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := at
	collections, waits := 0, 0
	collect := func(context.Context) (inspection.Result, error) {
		if collections != waits {
			t.Fatal("collection overlapped or caught up before waiting")
		}
		collections++
		// A very slow cycle must still be followed by one full interval.
		current = current.Add(time.Hour)
		r := observation()
		if collections == 3 {
			r.ID = strings.Repeat("b", 64)
			r.Health = "unknown"
			r.CheckTime = time.Time{}
		}
		return r, nil
	}
	wait := func(ctx context.Context, d time.Duration) error {
		if collections != waits+1 || d != cfg.Interval {
			t.Fatal("wait did not follow exactly one completed collection")
		}
		rows, err := store.History(ctx, cfg.DBPath, id, 20)
		if err != nil || len(rows) != min(collections, 2) {
			t.Fatalf("wait before persistence: %v %v", rows, err)
		}
		waits++
		if waits == 3 {
			cancel()
			return ctx.Err()
		}
		current = current.Add(d)
		return nil
	}
	if err := loop(ctx, cfg, collect, func() time.Time { return current }, wait, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if collections != 3 || waits != 3 {
		t.Fatal("wrong cycle count")
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 2 || !rows[0].CheckTime.Equal(rows[1].CheckTime) {
		t.Fatalf("repeated check timestamp lost: %v %v", rows, err)
	}
	rows, err = store.History(context.Background(), cfg.DBPath, strings.Repeat("b", 64), 20)
	if err != nil || len(rows) != 1 || rows[0].Name != "demo" || rows[0].Health != "unknown" || !rows[0].CheckTime.IsZero() {
		t.Fatalf("replacement history: %v %v", rows, err)
	}
}

func TestCollectionErrorContinuesAtCadence(t *testing.T) {
	cfg := config(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, waits := 0, 0
	collect := func(context.Context) (inspection.Result, error) {
		calls++
		if calls == 1 {
			return inspection.Result{}, errors.New("SECRET daemon body")
		}
		return observation(), nil
	}
	wait := func(ctx context.Context, d time.Duration) error {
		waits++
		if d != cfg.Interval || waits != calls {
			t.Fatal("error bypassed normal cadence")
		}
		if waits == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	var out, errOut bytes.Buffer
	if err := loop(ctx, cfg, collect, func() time.Time { return at }, wait, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	failures, err := store.ErrorHistory(context.Background(), cfg.DBPath, "demo", 20)
	if err != nil || len(failures) != 1 || failures[0].Target != "demo" || failures[0].Category != "request_failed" {
		t.Fatalf("errors=%v err=%v", failures, err)
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("error became a health observation: %v %v", rows, err)
	}
	if strings.Contains(errOut.String(), "SECRET") || !strings.Contains(out.String(), "Health: healthy") {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}

func TestPersistenceFailureIsFatal(t *testing.T) {
	for _, failCollection := range []bool{false, true} {
		t.Run(fmt.Sprint(failCollection), func(t *testing.T) {
			cfg := config(t)
			if err := os.WriteFile(cfg.DBPath, []byte("not SQLite"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			collect := func(context.Context) (inspection.Result, error) {
				calls++
				if failCollection {
					return inspection.Result{}, errors.New("collection failure")
				}
				return observation(), nil
			}
			wait := func(context.Context, time.Duration) error { t.Fatal("scheduled after persistence failure"); return nil }
			err := loop(context.Background(), cfg, collect, func() time.Time { return at }, wait, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "cannot persist history") || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			lock, err := acquireLock(cfg.DBPath)
			if err != nil {
				t.Fatal("fatal error retained lock:", err)
			}
			lock.Close()
		})
	}
}

func TestCanceledCollectionDoesNotSaveAnError(t *testing.T) {
	cfg := config(t)
	ctx, cancel := context.WithCancel(context.Background())
	collect := func(context.Context) (inspection.Result, error) { cancel(); return inspection.Result{}, ctx.Err() }
	if err := loop(ctx, cfg, collect, time.Now, waitInterval, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.DBPath); !os.IsNotExist(err) {
		t.Fatal("user cancellation created a database")
	}
	if err := waitInterval(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("waiting ignored cancellation")
	}
}

type notifyingWriter struct {
	bytes.Buffer
	once    sync.Once
	written chan struct{}
}

func (w *notifyingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.once.Do(func() { close(w.written) })
	return n, err
}

func fakeDocker(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" || r.URL.RequestURI() != "/v1.52/containers/demo/json" {
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(500)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	docker, err := client.New(client.WithHost(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	return docker
}

func TestRunReadOnlyHistoryAndSecondMonitor(t *testing.T) {
	cfg := config(t)
	cfg.Interval = time.Hour
	docker := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Id":%q,"Name":"/demo","Config":{"Healthcheck":{"Test":["CMD","true"]}},"State":{"Status":"running","Health":{"Status":"healthy","Log":[{"End":"2026-10-20T12:00:00Z"}]}}}`, id)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &notifyingWriter{written: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, docker, cfg, out, io.Discard) }()
	select {
	case <-out.written:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("no immediate inspection")
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("live read-only history: %v %v", rows, err)
	}
	other := cfg
	other.DBPath = filepath.Join(filepath.Dir(cfg.DBPath), "other.db")
	if err := Run(ctx, docker, other, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "another monitor") {
		t.Fatalf("second monitor: %v", err)
	}
	if _, err := os.Stat(other.DBPath); !os.IsNotExist(err) {
		t.Fatal("second monitor wrote before acquiring lock")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, err = store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 1 {
		t.Fatal("shutdown lost persisted history")
	}
}

func TestRunRequestTimeoutAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			cfg := config(t)
			cfg.Interval = time.Hour
			entered := make(chan struct{})
			docker := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errOut := &notifyingWriter{written: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, docker, cfg, io.Discard, errOut) }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("request not issued")
			}
			if !cancelRequest {
				select {
				case <-errOut.written:
				case <-time.After(10 * time.Second):
					cancel()
					<-done
					t.Fatal("request timeout not recorded")
				}
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if cancelRequest {
				if _, err := os.Stat(cfg.DBPath); !os.IsNotExist(err) {
					t.Fatal("cancellation recorded an application observation")
				}
			} else {
				failures, err := store.ErrorHistory(context.Background(), cfg.DBPath, "demo", 20)
				if err != nil || len(failures) != 1 || failures[0].Category != "timeout" {
					t.Fatalf("timeout: %v %v", failures, err)
				}
			}
		})
	}
}

func TestRunContinuesAfterMissingContainer(t *testing.T) {
	cfg := config(t)
	cfg.Interval = time.Nanosecond
	var requests atomic.Int32
	docker := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"SECRET"}`)
			return
		}
		fmt.Fprintf(w, `{"Id":%q,"Name":"/demo","Config":{},"State":{"Status":"running"}}`, id)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &notifyingWriter{written: make(chan struct{})}
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Run(ctx, docker, cfg, out, &errOut) }()
	select {
	case <-out.written:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("monitor did not continue after 404")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	failures, err := store.ErrorHistory(context.Background(), cfg.DBPath, "demo", 20)
	if err != nil || len(failures) != 1 || failures[0].Category != "not_found" {
		t.Fatalf("errors=%v err=%v", failures, err)
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) < 1 || rows[0].Health != "unknown" {
		t.Fatalf("health=%v err=%v", rows, err)
	}
	if strings.Contains(errOut.String(), "SECRET") {
		t.Fatal("daemon error leaked")
	}
}

func TestRunUnavailableDockerEndpoint(t *testing.T) {
	cfg := config(t)
	cfg.Interval = time.Hour
	docker, err := client.New(client.WithHost("unix://" + filepath.Join(t.TempDir(), "missing-docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errOut := &notifyingWriter{written: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, docker, cfg, io.Discard, errOut) }()
	select {
	case <-errOut.written:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("Docker unavailability was not recorded")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	failures, err := store.ErrorHistory(context.Background(), cfg.DBPath, "demo", 20)
	if err != nil || len(failures) != 1 || failures[0].Category != "unavailable" {
		t.Fatalf("errors=%v err=%v", failures, err)
	}
	rows, err := store.History(context.Background(), cfg.DBPath, id, 20)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unavailable Docker became unhealthy: %v %v", rows, err)
	}
}
