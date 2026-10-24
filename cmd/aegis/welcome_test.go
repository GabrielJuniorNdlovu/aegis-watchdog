package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/banner"
)

func TestWelcomeRedirectedOutputIsPlainTitle(t *testing.T) {
	var dockerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dockerCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer server.Close()
	dockerEnv(t, server.URL)

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"welcome"}, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if out.String() != banner.PlainTitle+"\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), banner.PlainTitle+"\n")
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "█") {
		t.Fatalf("redirected welcome contained decorative output: %q", out.String())
	}
	if dockerCalls.Load() != 0 {
		t.Fatalf("welcome contacted Docker %d times", dockerCalls.Load())
	}

	// Verify an *os.File backed by a regular file is also detected as non-TTY.
	file, err := os.Create(filepath.Join(t.TempDir(), "welcome.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if term := detectTerminal(file); term.isTTY {
		t.Fatalf("regular file detected as TTY: %+v", term)
	}
}

func TestWelcomeInteractiveOptionsAndFallbacks(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cases := []struct {
		name      string
		term      terminal
		vars      map[string]string
		wantMode  banner.ColorMode
		wantWidth int
		wantUni   bool
	}{
		{
			name:      "truecolor wide",
			term:      terminal{isTTY: true, width: 100},
			vars:      map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"},
			wantMode:  banner.TrueColor,
			wantWidth: 100,
			wantUni:   true,
		},
		{
			name:      "256color from TERM",
			term:      terminal{isTTY: true, width: 80},
			vars:      map[string]string{"TERM": "tmux-256color", "LANG": "C.UTF-8"},
			wantMode:  banner.Color256,
			wantWidth: 80,
			wantUni:   true,
		},
		{
			name:      "basic16 fallback and COLUMNS fallback",
			term:      terminal{isTTY: true, width: 0},
			vars:      map[string]string{"TERM": "xterm", "COLUMNS": "52", "LC_CTYPE": "en_US.utf8"},
			wantMode:  banner.Basic16,
			wantWidth: 52,
			wantUni:   true,
		},
		{
			name:      "NO_COLOR overrides truecolor",
			term:      terminal{isTTY: true, width: 90},
			vars:      map[string]string{"TERM": "xterm-256color", "COLORTERM": "24bit", "NO_COLOR": "1", "LANG": "en_US.UTF-8"},
			wantMode:  banner.NoColor,
			wantWidth: 90,
			wantUni:   true,
		},
		{
			name:      "dumb terminal disables unicode",
			term:      terminal{isTTY: true, width: 90},
			vars:      map[string]string{"TERM": "dumb", "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"},
			wantMode:  banner.TrueColor,
			wantWidth: 90,
			wantUni:   false,
		},
		{
			name:      "LC_ALL=C overrides LANG UTF-8",
			term:      terminal{isTTY: true, width: 90},
			vars:      map[string]string{"TERM": "xterm-256color", "LC_ALL": "C", "LANG": "en_US.UTF-8"},
			wantMode:  banner.Color256,
			wantWidth: 90,
			wantUni:   false,
		},
	}
	for _, tc := range cases {
		opts := welcomeOptions(env(tc.vars), tc.term)
		if opts.Color != tc.wantMode || opts.Width != tc.wantWidth || opts.Unicode != tc.wantUni {
			t.Errorf("%s: got %+v, want width=%d color=%v unicode=%v",
				tc.name, opts, tc.wantWidth, tc.wantMode, tc.wantUni)
		}
	}

	// End-to-end interactive welcomeCommand render with NO_COLOR has no escapes.
	var out, errOut bytes.Buffer
	code := welcomeCommand(nil, &out, &errOut, env(map[string]string{
		"TERM": "xterm-256color", "NO_COLOR": "1", "LANG": "en_US.UTF-8",
	}), terminal{isTTY: true, width: 80})
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), banner.Subtitle) {
		t.Fatalf("unexpected NO_COLOR TTY output:\n%s", out.String())
	}
}

func TestWelcomeArgumentsAndWriteFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"welcome", "--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "aegis welcome") || errOut.Len() != 0 {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"welcome", "extra"}, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("extra arg: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := welcomeCommand(nil, brokenOutput{}, &errOut, func(string) string { return "" }, terminal{}); code != 1 || !strings.Contains(errOut.String(), "cannot write welcome output") {
		t.Fatalf("write error: code=%d stderr=%q", code, errOut.String())
	}
}

func TestExistingCommandsDoNotEmitBanner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.52")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Id":"` + strings.Repeat("a", 64) + `","Name":"/demo","Config":{},"State":{"Status":"running"}}`))
	}))
	defer server.Close()
	dockerEnv(t, server.URL)

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "--container", "demo"}, &out, &errOut); code != 0 {
		t.Fatalf("inspect failed: code=%d stderr=%q", code, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "█") || strings.Contains(out.String(), banner.Subtitle) {
		t.Fatalf("inspect output contained welcome banner:\n%s", out.String())
	}
}
