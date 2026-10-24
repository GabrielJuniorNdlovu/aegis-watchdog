package banner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLayoutBoundaries(t *testing.T) {
	if WideWidth != 76 {
		t.Fatalf("WideWidth = %d, want 76", WideWidth)
	}
	if CompactWidth != 37 {
		t.Fatalf("CompactWidth = %d, want 37", CompactWidth)
	}
	cases := []struct {
		opts Options
		want Layout
	}{
		{Options{Width: WideWidth, Unicode: true}, Wide},
		{Options{Width: WideWidth + 40, Unicode: true}, Wide},
		{Options{Width: WideWidth - 1, Unicode: true}, Compact},
		{Options{Width: CompactWidth, Unicode: true}, Compact},
		{Options{Width: CompactWidth - 1, Unicode: true}, Plain},
		{Options{Width: 1, Unicode: true}, Plain},
		{Options{Width: 0, Unicode: true}, Plain},
		{Options{Width: 120, Unicode: false}, Plain},
	}
	for _, tc := range cases {
		if got := Choose(tc.opts); got != tc.want {
			t.Errorf("Choose(%+v) = %v, want %v", tc.opts, got, tc.want)
		}
	}
}

func TestNoLineExceedsTerminalWidth(t *testing.T) {
	modes := []ColorMode{NoColor, Basic16, Color256, TrueColor}
	widths := []int{1, 2, 3, 4, 5, 8, 20, 30, CompactWidth - 1, CompactWidth, 50, WideWidth - 1, WideWidth, 80, 120}
	for _, unicode := range []bool{false, true} {
		for _, mode := range modes {
			for _, width := range widths {
				out := Render(Options{Width: width, Color: mode, Unicode: unicode})
				if !strings.HasSuffix(out, "\n") {
					t.Fatalf("output for width=%d mode=%d missing trailing newline", width, mode)
				}
				lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
				if unicode && width >= WideWidth && (len(lines) < 10 || len(lines) > 12) {
					t.Fatalf("wide banner height = %d rows, want 10-12", len(lines))
				}
				for i, line := range lines {
					if got := VisibleWidth(line); got > width {
						t.Errorf("unicode=%v mode=%d width=%d line %d visible width = %d > %d: %q",
							unicode, mode, width, i+1, got, width, line)
					}
				}
			}
		}
	}
}

func TestVisibleWidthIgnoresANSIAndCountsRunes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"AEGIS", 5},
		{"\x1b[38;2;162;127;224m███\x1b[38;5;140m░░░\x1b[0m", 6},
		{"\x1b[95m▚▗▖▞\x1b[0m", 4},
	}
	for _, tc := range cases {
		if got := VisibleWidth(tc.in); got != tc.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, width := range []int{CompactWidth, WideWidth} {
		plainOut := strings.Split(strings.TrimSuffix(Render(Options{Width: width, Color: NoColor, Unicode: true}), "\n"), "\n")
		colorOut := strings.Split(strings.TrimSuffix(Render(Options{Width: width, Color: TrueColor, Unicode: true}), "\n"), "\n")
		if len(plainOut) != len(colorOut) {
			t.Fatalf("width %d: plain lines=%d color lines=%d", width, len(plainOut), len(colorOut))
		}
		for i := range colorOut {
			if strings.Contains(colorOut[i], "\x1b[") && len(colorOut[i]) <= utf8.RuneCountInString(plainOut[i]) {
				t.Fatalf("width %d line %d expected ANSI bytes in truecolor line", width, i+1)
			}
			// Monochrome hollows the eyes and beak, so visible width on rows without
			// trailing hollow cells matches the stripped rune count.
			if got := VisibleWidth(colorOut[i]); got != utf8.RuneCountInString(plainOut[i]) {
				t.Errorf("width %d line %d VisibleWidth(color)=%d, plain runes=%d", width, i+1, got, utf8.RuneCountInString(plainOut[i]))
			}
		}
	}
}

func TestNoColorAndNonUnicodeContainNoANSIEscapes(t *testing.T) {
	for _, width := range []int{25, CompactWidth, WideWidth, 100} {
		mono := Render(Options{Width: width, Color: NoColor, Unicode: true})
		if strings.Contains(mono, "\x1b") {
			t.Errorf("NoColor width=%d contained ESC: %q", width, mono)
		}
		ascii := Render(Options{Width: width, Color: TrueColor, Unicode: false})
		if strings.Contains(ascii, "\x1b") {
			t.Errorf("non-Unicode width=%d contained ESC: %q", width, ascii)
		}
		for _, r := range ascii {
			if r > 0x7f {
				t.Errorf("non-Unicode width=%d contained non-ASCII rune %U", width, r)
			}
		}
	}
}

func TestColorModesEmitOnlyForegroundSGR(t *testing.T) {
	tc := Render(Options{Width: WideWidth, Color: TrueColor, Unicode: true})
	if !strings.Contains(tc, "\x1b[38;2;") {
		t.Error("TrueColor output missing 24-bit SGR sequence")
	}
	c256 := Render(Options{Width: WideWidth, Color: Color256, Unicode: true})
	if !strings.Contains(c256, "\x1b[38;5;") || strings.Contains(c256, "\x1b[38;2;") {
		t.Error("Color256 output did not use 256-color SGR sequences exclusively")
	}
	b16 := Render(Options{Width: WideWidth, Color: Basic16, Unicode: true})
	if !strings.Contains(b16, "\x1b[95m") || strings.Contains(b16, "\x1b[38;") {
		t.Error("Basic16 output did not use 16-color SGR sequences exclusively")
	}
	for _, out := range []string{tc, c256, b16} {
		if strings.Contains(out, "\x1b[48;") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b[H") {
			t.Error("banner must not set background color or manipulate the screen")
		}
	}
}

func TestGoldenArtwork(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"wide.golden", Options{Width: WideWidth, Color: NoColor, Unicode: true}},
		{"compact.golden", Options{Width: CompactWidth, Color: NoColor, Unicode: true}},
		{"plain.golden", Options{Width: 30, Color: NoColor, Unicode: false}},
	}
	for _, tc := range cases {
		path := filepath.Join("testdata", tc.name)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if got := Render(tc.opts); got != string(want) {
			t.Errorf("%s mismatch:\ngot:\n%s\nwant:\n%s", tc.name, got, string(want))
		}
	}
}
