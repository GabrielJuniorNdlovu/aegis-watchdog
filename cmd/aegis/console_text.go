package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// parseConsoleLine handles only words, quotes, and escapes. It never invokes
// a shell or expands variables, substitutions, globs, or redirects.
func parseConsoleLine(line string) ([]string, error) {
	if len(line) > 4096 {
		return nil, errors.New("command exceeds 4096 bytes")
	}
	var args []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	flush := func() {
		if started {
			args = append(args, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range line {
		if (unicode.IsControl(r) && r != '\t') || unicode.Is(unicode.Cf, r) {
			return nil, errors.New("command contains terminal control characters")
		}
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unfinished quote or escape")
	}
	flush()
	if len(args) > 64 {
		return nil, errors.New("command exceeds 64 arguments")
	}
	return args, nil
}

const consoleOutputLimit = 64 * 1024

var errConsoleOutputLimit = errors.New("console output limit reached")

type consoleOutput struct {
	writer    io.Writer
	remaining int
	err       error
}

// Only trusted artwork and the line editor may emit terminal controls.
// Escaping the control rune itself also neutralizes split ANSI/OSC sequences.
func (w *consoleOutput) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	var safe strings.Builder
	for _, r := range string(p) {
		if r == '\n' {
			safe.WriteRune(r)
		} else if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&safe, "\\u%04x", r)
		} else {
			safe.WriteRune(r)
		}
	}
	text := safe.String()
	if len(text) > w.remaining {
		text = strings.ToValidUTF8(text[:w.remaining], "") + "\n[Console output truncated at 64 KiB; use the one-shot command for full output.]\n"
		w.err = errConsoleOutputLimit
	}
	n, err := io.WriteString(w.writer, text)
	w.remaining -= min(n, w.remaining)
	if err != nil {
		w.err = err
		return 0, err
	}
	if n != len(text) {
		w.err = io.ErrShortWrite
		return 0, w.err
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}
