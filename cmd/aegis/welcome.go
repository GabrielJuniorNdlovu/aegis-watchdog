package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/banner"
	"golang.org/x/sys/unix"
)

const welcomeUsage = "  aegis welcome\n"

// terminal describes stdout for the welcome command.
type terminal struct {
	isTTY bool
	width int // columns reported by the terminal; 0 when unknown
}

// detectTerminal reports whether w is a terminal and its width. A successful
// TIOCGWINSZ ioctl implies a terminal; pipes, files, and buffers fail it.
func detectTerminal(w io.Writer) terminal {
	f, ok := w.(*os.File)
	if !ok {
		return terminal{}
	}
	size, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return terminal{}
	}
	return terminal{isTTY: true, width: int(size.Col)}
}

// welcomeCommand prints the branded header. It reads no configuration and
// touches neither Docker nor SQLite.
func welcomeCommand(args []string, stdout, stderr io.Writer, getenv func(string) string, term terminal) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	output := banner.PlainTitle + "\n"
	if term.isTTY {
		output = banner.Render(welcomeOptions(getenv, term))
	}
	if _, err := io.WriteString(stdout, output); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write welcome output")
		return 1
	}
	return 0
}

// welcomeOptions maps the environment to rendering options. Dumb terminals
// and non-UTF-8 locales get ASCII text; NO_COLOR disables escape sequences.
func welcomeOptions(getenv func(string) string, term terminal) banner.Options {
	width := term.width
	if width <= 0 {
		width, _ = strconv.Atoi(getenv("COLUMNS"))
	}
	if width <= 0 {
		width = 80
	}
	termName := getenv("TERM")
	opts := banner.Options{Width: width, Unicode: termName != "" && termName != "dumb" && utf8Locale(getenv)}
	colorTerm := strings.ToLower(getenv("COLORTERM"))
	switch {
	case getenv("NO_COLOR") != "":
		opts.Color = banner.NoColor
	case colorTerm == "truecolor" || colorTerm == "24bit":
		opts.Color = banner.TrueColor
	case strings.Contains(termName, "256color"):
		opts.Color = banner.Color256
	default:
		opts.Color = banner.Basic16
	}
	return opts
}

// utf8Locale applies POSIX precedence: LC_ALL, then LC_CTYPE, then LANG.
func utf8Locale(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := getenv(name); value != "" {
			value = strings.ToLower(value)
			return strings.Contains(value, "utf-8") || strings.Contains(value, "utf8")
		}
	}
	return false
}
