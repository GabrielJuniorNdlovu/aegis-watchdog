package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/readline"
)

func TestConsoleParsing(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"", nil},
		{`/inspect 'my app' --db "data/a db"`, []string{"/inspect", "my app", "--db", "data/a db"}},
		{`/help a\ b ''`, []string{"/help", "a b", ""}},
		{`/inspect $(touch /tmp/nope); * > file`, []string{"/inspect", "$(touch", "/tmp/nope);", "*", ">", "file"}},
	} {
		got, err := parseConsoleLine(tc.line)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parse %q: %q %v", tc.line, got, err)
		}
	}
	for _, line := range []string{`/inspect "unfinished`, `/help\`, "/help\n/exit", "/help\x1b[2J", "/help\u202e", strings.Repeat("x", 4097), strings.Repeat("x ", 65)} {
		if _, err := parseConsoleLine(line); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
}

func TestConsoleOutputNeutralizesTerminalControlsAndBoundsOutput(t *testing.T) {
	var out bytes.Buffer
	w := &consoleOutput{writer: &out, remaining: consoleOutputLimit}
	for _, chunk := range []string{"owl ▝\n", "\x1b", "[2J\x1b]52;c;secret\a\r\b\u009b31m\u202e"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	if strings.ContainsAny(got, "\x1b\a\r\b\u009b\u202e") || !strings.Contains(got, "owl ▝\n") || !strings.Contains(got, `\u001b[2J`) {
		t.Fatal(got)
	}
	out.Reset()
	w = &consoleOutput{writer: &out, remaining: consoleOutputLimit}
	if _, err := io.WriteString(w, strings.Repeat("x", consoleOutputLimit+1)); !errors.Is(err, errConsoleOutputLimit) {
		t.Fatal(err)
	}
	if out.Len() > consoleOutputLimit+100 || !strings.Contains(out.String(), "truncated") {
		t.Fatal(out.Len())
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, errConsoleOutputLimit) {
		t.Fatal(err)
	}
	w = &consoleOutput{writer: brokenOutput{}, remaining: 100}
	if _, err := w.Write([]byte("output")); err == nil {
		t.Fatal("write failure lost")
	}
}

type consoleStep struct {
	line string
	err  error
}
type scriptedConsole struct {
	steps []consoleStep
	reads int
}

func (s *scriptedConsole) ReadLine() (string, error) {
	s.reads++
	if len(s.steps) == 0 {
		return "", io.EOF
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step.line, step.err
}
func (s *scriptedConsole) Close() error { return nil }

func TestConsoleLoopRecoversAndExits(t *testing.T) {
	var out, errOut bytes.Buffer
	input := &scriptedConsole{steps: []consoleStep{{err: readline.ErrInterrupt}, {line: "/unknown"}, {line: `/help "`}, {line: "/exit extra"}, {line: "/help"}, {line: "/quit"}}}
	if code := consoleLoop(context.Background(), input, "unused.db", &out, &errOut); code != 0 || errOut.Len() != 0 || input.reads != 6 {
		t.Fatal(code, errOut.String(), input.reads)
	}
	for _, part := range []string{"Unknown command", "unfinished quote", "Usage: /exit", "Console commands:"} {
		if !strings.Contains(out.String(), part) {
			t.Fatal(part, out.String())
		}
	}
	input = &scriptedConsole{steps: []consoleStep{{err: errors.New("read failed")}}}
	if code := consoleLoop(context.Background(), input, "unused.db", &out, &errOut); code != 1 {
		t.Fatal(code)
	}
}

type blockingConsole struct {
	closed  chan struct{}
	started chan struct{}
	once    sync.Once
}

func (b *blockingConsole) ReadLine() (string, error) { close(b.started); <-b.closed; return "", io.EOF }
func (b *blockingConsole) Close() error              { b.once.Do(func() { close(b.closed) }); return nil }
func TestConsoleParentCancellationClosesIdleInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &blockingConsole{closed: make(chan struct{}), started: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- consoleLoop(ctx, input, "unused.db", io.Discard, io.Discard) }()
	<-input.started
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("console did not unblock on shutdown")
	}
}

func TestConsoleLaunchValidationHasNoSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "history.db")
	var out, errOut bytes.Buffer
	for _, args := range [][]string{{"--help"}, {"extra"}, {"--db", ""}, {"--db", path}} {
		out.Reset()
		errOut.Reset()
		code := consoleCommand(context.Background(), args, os.Stdin, &out, &errOut)
		if len(args) == 1 && args[0] == "--help" {
			if code != 0 || !strings.Contains(out.String(), "aegis console") {
				t.Fatal(code, out.String())
			}
		} else if code != 2 {
			t.Fatal(args, code)
		}
		if strings.Contains(out.String(), "█") {
			t.Fatal("non-interactive launch emitted artwork")
		}
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("console created its data directory", err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"console"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "one-shot") {
		t.Fatal(code, errOut.String())
	}
}
