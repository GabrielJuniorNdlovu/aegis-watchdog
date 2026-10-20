package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func TestErrorsCommandWithoutDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.db")
	if err := store.SaveCollectionError(context.Background(), path, "app", historyTime, errors.New("SECRET")); err != nil {
		t.Fatal(err)
	}
	dockerEnv(t, "invalid-Docker-host")
	for _, target := range []string{"app", "absent"} {
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"errors", "--container", target, "--db", path}, &out, &errOut)
		if code != 0 || errOut.Len() != 0 || strings.Contains(out.String(), "SECRET") {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
		if target == "app" && (!strings.Contains(out.String(), "not application health results") || !strings.Contains(out.String(), "Category: request_failed")) {
			t.Fatal(out.String())
		}
		if target == "absent" && !strings.Contains(out.String(), "No saved collection errors") {
			t.Fatal(out.String())
		}
	}
}
