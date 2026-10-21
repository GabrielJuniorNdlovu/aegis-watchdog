package evidence

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/budget"
	"github.com/moby/moby/client"
)

var at = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
var id = strings.Repeat("a", 64)

func input() Input {
	return Input{ActionID: 1, ServiceID: "demo", ContainerID: id, ReservedAt: at, Policy: budget.DefaultPolicy(), Assessments: []Assessment{{EvaluatedAt: at, Status: "persistent failure", Reason: "check_persistent_failure"}}}
}
func frame(s string) []byte {
	var h [8]byte
	h[0] = 1
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h[:], s...)
}

type countedReader struct {
	io.Reader
	n int
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}
func TestLogRedactionFramingAndBounds(t *testing.T) {
	text := "hello\nAuthorization: Bearer DO_NOT_KEEP\npassword=HIDDEN token=TOKEN api_key=KEY\nhttps://user:PASS@example.invalid/\n"
	for _, tty := range []bool{false, true} {
		raw := []byte(text)
		if !tty {
			raw = append(frame(text[:17]), frame(text[17:])...)
		}
		logs, truncated, err := readLogs(bytes.NewReader(raw), tty, 4096)
		if err != nil || truncated {
			t.Fatal(logs, truncated, err)
		}
		for _, secret := range []string{"DO_NOT_KEEP", "HIDDEN", "TOKEN", "KEY", "PASS"} {
			if strings.Contains(logs, secret) {
				t.Fatal("secret retained", logs)
			}
		}
		if !strings.Contains(logs, "hello") || !strings.Contains(logs, "[REDACTED]") {
			t.Fatal(logs)
		}
	}
	reader := &countedReader{Reader: strings.NewReader(strings.Repeat("x", 1000000))}
	logs, truncated, _ := readLogs(reader, true, 8192)
	if reader.n > 8192 || !truncated || logs != "" {
		t.Fatal(reader.n, truncated, len(logs))
	}
	logs, truncated, err := readLogs(strings.NewReader(strings.Repeat("x", 5000)+"password=SECRET\nnext safe\n"), true, 8192)
	if err != nil || !truncated || logs != "next safe\n" {
		t.Fatal(logs, truncated, err)
	}
	if logs, _, _ := readLogs(strings.NewReader("password=unfinished"), true, 100); logs != "" {
		t.Fatal("unfinished secret written")
	}
	if _, _, err := readLogs(strings.NewReader("badframe"), false, 100); err == nil {
		t.Fatal("malformed framing accepted")
	}
}
func fakeDocker(t *testing.T, tty bool, handler func(http.ResponseWriter, *http.Request, int)) *client.Client {
	t.Helper()
	inspections := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		if r.Method != "GET" {
			t.Errorf("unexpected mutation: %s", r.Method)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/v1.52/containers/" + id + "/json":
			inspections++
			handler(w, r, inspections)
		case "/v1.52/containers/" + id + "/logs":
			if r.URL.Query().Get("follow") != "" || r.URL.Query().Get("tail") != "200" || r.URL.Query().Get("since") == "" || r.URL.Query().Get("until") == "" {
				t.Error("unbounded log request", r.URL.RawQuery)
			}
			handler(w, r, inspections)
		default:
			t.Errorf("wrong target: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	docker, err := client.New(client.WithHost(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	return docker
}
func body(tty bool, start time.Time) string {
	return fmt.Sprintf(`{"Id":%q,"Name":"/demo","Config":{"Tty":%t,"Env":["TOKEN=ENV_SECRET"],"Healthcheck":{"Test":["CMD","secret command"]}},"State":{"Status":"running","StartedAt":%q,"Health":{"Status":"healthy","Log":[{"Start":"2026-10-21T11:59:00Z","End":"2026-10-21T11:59:01Z","ExitCode":0,"Output":"PROBE_SECRET"}]}}}`, id, tty, start.Format(time.RFC3339Nano))
}
func TestCollectAllowlistTTYAndExecutionChanges(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprint(tty), func(t *testing.T) {
			docker := fakeDocker(t, tty, func(w http.ResponseWriter, r *http.Request, n int) {
				if strings.HasSuffix(r.URL.Path, "/logs") {
					data := []byte("password=LOG_SECRET\nsafe\n")
					if !tty {
						data = frame(string(data))
					}
					w.Write(data)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body(tty, at.Add(-time.Hour)))
			})
			b, err := Collect(context.Background(), docker, input(), DefaultLimits(), func() time.Time { return at })
			if err != nil || b.Status != "complete" || b.Before.ID != id || b.After.ID != id {
				t.Fatal(b, err)
			}
			encoded, _ := json.Marshal(b)
			for _, s := range []string{"ENV_SECRET", "PROBE_SECRET", "LOG_SECRET", "secret command"} {
				if bytes.Contains(encoded, []byte(s)) {
					t.Fatal("secret persisted in schema")
				}
			}
		})
	}
	docker := fakeDocker(t, true, func(w http.ResponseWriter, r *http.Request, n int) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			fmt.Fprintln(w, "safe")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body(true, at.Add(time.Duration(n)*time.Second)))
	})
	b, err := Collect(context.Background(), docker, input(), DefaultLimits(), func() time.Time { return at })
	if err != nil || b.Status != "partial" || !strings.Contains(strings.Join(b.Issues, ","), "execution_changed") {
		t.Fatal(b, err)
	}
}
func TestCollectMissingAndDeadlineClosesStream(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			closed := make(chan struct{}, 1)
			docker := fakeDocker(t, true, func(w http.ResponseWriter, r *http.Request, n int) {
				if missing {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(404)
					fmt.Fprint(w, `{"message":"SECRET"}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/logs") {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					closed <- struct{}{}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body(true, at.Add(-time.Hour)))
			})
			p := DefaultLimits()
			p.Deadline = 100 * time.Millisecond
			b, err := Collect(context.Background(), docker, input(), p, func() time.Time { return at })
			if err != nil || b.Status != "partial" {
				t.Fatal(b, err)
			}
			if !missing {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("cancellation did not close stream")
				}
			}
		})
	}
}
func TestLimits(t *testing.T) {
	p := DefaultLimits()
	if p.Validate() != nil {
		t.Fatal(p)
	}
	p.BundleBytes = 1024
	if p.Validate() == nil {
		t.Fatal("JSON overhead ignored")
	}
}
