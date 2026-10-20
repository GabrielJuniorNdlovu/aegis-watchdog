package main

import (
	"bytes"
	"context"
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
