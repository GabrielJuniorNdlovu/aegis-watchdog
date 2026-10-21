package evidence

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestPublicationWriteFailure(t *testing.T) {
	if os.Getenv("AEGIS_TEST_FILE_SIZE_LIMIT") == "1" {
		path := filepath.Join(t.TempDir(), "aegis.db")
		d, err := openDirectory(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer d.root.Close()
		signal.Ignore(syscall.SIGXFSZ)
		if err = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1024, Max: 1024}); err != nil {
			t.Fatal(err)
		}
		if err = d.publish(1, bytes.Repeat([]byte("redacted "), 1024)); err == nil {
			t.Fatal("disk write limit was ignored")
		}
		final, temp := names(1)
		for _, name := range []string{final, temp} {
			if _, err = d.root.Lstat(name); !os.IsNotExist(err) {
				t.Fatal("failed publication left a file", name, err)
			}
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestPublicationWriteFailure$")
	command.Env = append(os.Environ(), "AEGIS_TEST_FILE_SIZE_LIMIT=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write failure subprocess: %s: %v", output, err)
	}
}

func TestPublicationDoesNotReplaceExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aegis.db")
	d, err := openDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	final, temp := names(1)
	if err = os.WriteFile(filepath.Join(d.path, final), []byte("ORIGINAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = d.publish(1, []byte("new redacted content")); err == nil {
		t.Fatal("existing publication replaced")
	}
	raw, err := d.read(final, 100)
	if err != nil || string(raw) != "ORIGINAL" {
		t.Fatal(string(raw), err)
	}
	if _, err = d.root.Lstat(temp); !os.IsNotExist(err) {
		t.Fatal("unpublished temporary file retained", err)
	}
}

func TestIntentPrecedesDockerAndFailedIntentMakesNoRequests(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			path := fixture(t)
			var calls atomic.Int32
			if reject {
				execSQL(t, path, `CREATE TRIGGER fail_intent BEFORE INSERT ON evidence_captures BEGIN SELECT RAISE(FAIL,'injected intent failure'); END;`)
			}
			docker := fakeDocker(t, false, func(w http.ResponseWriter, r *http.Request, _ int) {
				calls.Add(1)
				if saved := record(t, path, 1); saved.Status != "pending" {
					t.Error("Docker called before pending intent", saved.Status)
				}
				if strings.HasSuffix(r.URL.Path, "/logs") {
					w.Write(frame("safe synthetic log\n"))
				} else {
					fmt.Fprint(w, body(false, at.Add(-time.Hour)))
				}
			})
			_, err := Capture(context.Background(), path, 1, docker, DefaultLimits(), func() time.Time { return at })
			if reject {
				if err == nil || calls.Load() != 0 {
					t.Fatal(err, calls.Load())
				}
			} else if err != nil || calls.Load() != 3 {
				t.Fatal(err, calls.Load())
			}
			count := calls.Load()
			if _, err = Capture(context.Background(), path, 999, docker, DefaultLimits(), func() time.Time { return at }); err == nil || calls.Load() != count {
				t.Fatal("missing action captured", err)
			}
		})
	}
}
