package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMonitorArgumentsAndCancellation(t *testing.T) {
	dockerEnv(t, "invalid-Docker-host")
	for _, args := range [][]string{
		{"monitor"}, {"monitor", "--container", "demo", "--interval", "0"},
		{"monitor", "--container", "demo", "--interval", "-1s"},
		{"monitor", "--container", "demo", "--interval", "wrong"},
		{"monitor", "--container", "demo", "--db", ""},
		{"monitor", "--container", "demo", "--retention", "0"},
		{"monitor", "--container", "demo", "--retention", "-1h"},
		{"monitor", "--container", "demo", "--retention", "wrong"},
	} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("args=%q code=%d out=%q err=%q", args, code, out.String(), errOut.String())
		}
	}
	inspectionAPI(t, "healthy", 200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"monitor", "--container", "demo"}, &out, &errOut); code != 0 || strings.Contains(errOut.String(), "aegis:") {
		t.Fatalf("shutdown code=%d err=%q", code, errOut.String())
	}
}

func TestMonitorDatabaseFailureExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	inspectionAPI(t, "healthy", 200)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"monitor", "--container", "demo", "--db", path}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "cannot persist history") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}
