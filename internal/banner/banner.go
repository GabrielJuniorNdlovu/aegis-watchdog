// Package banner renders the decorative Aegis welcome header.
//
// Rendering is a pure function of width, color mode, and Unicode support so
// it can be tested without a terminal. Every glyph used in the artwork
// occupies one column in non-CJK terminals, which lets VisibleWidth count
// columns without a display-width dependency.
package banner

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Subtitle is the tagline shown beneath the wordmark.
const Subtitle = "Container health. Controlled recovery."

// PlainTitle is the single line written when output is not a terminal.
const PlainTitle = "Aegis - " + Subtitle

// ColorMode is the color capability used for rendering.
type ColorMode int

const (
	NoColor   ColorMode = iota // monochrome: shapes only, no escape sequences
	Basic16                    // standard 16-color SGR codes
	Color256                   // xterm 256-color palette
	TrueColor                  // 24-bit RGB
)

// Layout identifies the arrangement chosen for a width.
type Layout int

const (
	Plain   Layout = iota // simple wrapped text
	Compact               // small owl beside plain AEGIS text
	Wide                  // owl beside the large block wordmark
)

func (l Layout) String() string { return [...]string{"plain", "compact", "wide"}[l] }

// Options controls rendering.
type Options struct {
	Width   int // available terminal columns
	Color   ColorMode
	Unicode bool // false forces the ASCII plain layout
}

// rgb is a palette entry with explicit 256-color and 16-color fallbacks so
// dark purples stay purple instead of collapsing to gray or black.
type rgb struct {
	r, g, b uint8
	c256    int
	basic   int
}

var (
	lavender = rgb{0xA2, 0x7F, 0xE0, 140, 95}
	gold     = rgb{0xCF, 0x8F, 0x22, 172, 93}
	edge     = rgb{0xA4, 0x7D, 0xC9, 140, 95} // light purple rim: contrast on dark backgrounds
	head     = rgb{0x4E, 0x25, 0x5C, 53, 35}
	body     = rgb{0x66, 0x33, 0x78, 90, 35}
	wing     = rgb{0x43, 0x1F, 0x50, 53, 35}
	eye      = rgb{0xF0, 0xA0, 0x4B, 215, 93}
	iris     = rgb{0xD9, 0x78, 0x2E, 172, 33}
	pupil    = rgb{0x3A, 0x1A, 0x44, 235, 90}
	beak     = rgb{0xF0, 0x6A, 0x6E, 203, 91}
)

var owlPalette = map[byte]rgb{'l': edge, 'h': head, 'b': body, 'w': wing, 'e': eye, 'i': iris, 'p': pupil, 'k': beak}

// In monochrome these classes become blank cells so the eyes and beak remain
// visible as cut-outs instead of merging into the solid head.
const monoHollow = "eik"

// art pairs glyph rows with a parallel row of palette classes (one byte per
// column). Color changes inside the owl fall on cell boundaries, so no cell
// ever needs a background color.
type art struct{ glyphs, classes []string }

// owlLarge is 20 columns by 11 rows.
var owlLarge = art{
	glyphs: []string{
		"        ▚▗▖▞        ",
		"     ▄▄▄▄██▄▄▄▄     ",
		"   ▄████████████▄   ",
		"  ████████████████  ",
		" ██████████████████ ",
		" ██████████████████ ",
		" ██████████████████ ",
		"████████████████████",
		"████████████████████",
		"████████████████████",
		" ▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀ ",
	},
	classes: []string{
		"        llll        ",
		"     llllllllll     ",
		"   llhhhhhhhhhhll   ",
		"  lheeeehhhheeeehl  ",
		" lheippiehheippiehl ",
		" lhheeeehkkheeeehhl ",
		" lbhhhhhhhhhhhhhhbl ",
		"lbbbbhhhhhhhhhhbwwwl",
		"lbbbbbbbbbbbbbwwwwwl",
		"lbbbbbbbbbbbbbwwwwwl",
		" lbbbbbbbbbbbbwwwwl ",
	},
}

// owlSmall is 13 columns by 7 rows.
var owlSmall = art{
	glyphs: []string{
		"    ▄▄█▄▄    ",
		"  ▄███████▄  ",
		" ███████████ ",
		" ███████████ ",
		"█████████████",
		"█████████████",
		" ▀▀▀▀▀▀▀▀▀▀▀ ",
	},
	classes: []string{
		"    lllll    ",
		"  llhhhhhll  ",
		" leeeeheeeel ",
		" leppeheppel ",
		"lheeehkheeehl",
		"lbbhhhhhhhwwl",
		" lbbbbbbbbwl ",
	},
}

// letters are 7-row block glyphs for the wordmark; '#' is a filled cell.
var letters = map[byte][]string{
	'A': {" ####### ", "###   ###", "###   ###", "#########", "###   ###", "###   ###", "###   ###"},
	'E': {"########", "###     ", "###     ", "####### ", "###     ", "###     ", "########"},
	'G': {" ########", "###      ", "###      ", "###  ####", "###   ###", "###   ###", " ########"},
	'I': {"#######", "  ###  ", "  ###  ", "  ###  ", "  ###  ", "  ###  ", "#######"},
	'S': {" ########", "###      ", "###      ", " ####### ", "      ###", "      ###", "######## "},
}

const (
	word       = "AEGIS"
	letterGap  = 2 // columns between letters
	margin     = 1 // left margin before all artwork
	wideGap    = 4 // columns between owl and wordmark
	compactGap = 3 // columns between small owl and text
)

var compactLines = []string{"Container health.", "Controlled recovery."}

// cell is one terminal column. Uncolored cells use the terminal's default
// foreground; no cell ever sets a background color.
type cell struct {
	r       rune
	c       rgb
	colored bool
}

type canvas [][]cell

func newCanvas(rows, cols int) canvas {
	c := make(canvas, rows)
	for y := range c {
		c[y] = make([]cell, cols)
		for x := range c[y] {
			c[y][x].r = ' '
		}
	}
	return c
}

// blit copies src onto c with its top-left corner at (row, col).
func (c canvas) blit(src canvas, row, col int) {
	for y, line := range src {
		for x, v := range line {
			if v.r != ' ' {
				c[row+y][col+x] = v
			}
		}
	}
}

// text places uncolored (or uniformly colored) text.
func (c canvas) text(s string, row, col int, color *rgb) {
	for i, r := range []rune(s) {
		c[row][col+i] = cell{r: r}
		if color != nil {
			c[row][col+i] = cell{r, *color, true}
		}
	}
}

func (a art) width() int  { return utf8.RuneCountInString(a.glyphs[0]) }
func (a art) height() int { return len(a.glyphs) }

func (a art) canvas(mono bool) canvas {
	c := newCanvas(a.height(), a.width())
	for y, line := range a.glyphs {
		for x, r := range []rune(line) {
			class := a.classes[y][x]
			if r == ' ' || (mono && strings.IndexByte(monoHollow, class) >= 0) {
				continue
			}
			c[y][x] = cell{r, owlPalette[class], true}
		}
	}
	return c
}

// wordmark draws AEGIS in block letters with a ░ shadow one row down and one
// column left, colored by a lavender-to-gold horizontal gradient.
func wordmark() canvas {
	width := 1 // leading column for the shadow
	for i := range len(word) {
		if i > 0 {
			width += letterGap
		}
		width += len(letters[word[i]][0])
	}
	const rows = 7
	filled := make([][]bool, rows)
	for y := range filled {
		filled[y] = make([]bool, width)
	}
	x0 := 1
	for i := range len(word) {
		glyph := letters[word[i]]
		for y, line := range glyph {
			for dx := range len(line) {
				filled[y][x0+dx] = line[dx] == '#'
			}
		}
		x0 += len(glyph[0]) + letterGap
	}
	c := newCanvas(rows+1, width)
	for y := 0; y <= rows; y++ {
		for x := range width {
			col := gradient(float64(x) / float64(width-1))
			switch {
			case y < rows && filled[y][x]:
				c[y][x] = cell{'█', col, true}
			case y > 0 && x+1 < width && filled[y-1][x+1]:
				c[y][x] = cell{'░', col, true}
			}
		}
	}
	return c
}

func gradient(t float64) rgb {
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5) }
	out := rgb{r: lerp(lavender.r, gold.r), g: lerp(lavender.g, gold.g), b: lerp(lavender.b, gold.b), basic: lavender.basic}
	out.c256 = xterm256(out)
	if t >= 0.5 {
		out.basic = gold.basic
	}
	return out
}

var (
	wordmarkArt = wordmark()
	// WideWidth and CompactWidth are the minimum columns for each layout,
	// derived from the artwork itself.
	WideWidth    = margin + owlLarge.width() + wideGap + len(wordmarkArt[0])
	CompactWidth = margin + owlSmall.width() + compactGap + maxLen(append([]string{word}, compactLines...))
)

func maxLen(lines []string) int {
	n := 0
	for _, s := range lines {
		n = max(n, utf8.RuneCountInString(s))
	}
	return n
}

// Choose returns the layout used for opts.
func Choose(opts Options) Layout {
	switch {
	case !opts.Unicode:
		return Plain
	case opts.Width >= WideWidth:
		return Wide
	case opts.Width >= CompactWidth:
		return Compact
	default:
		return Plain
	}
}

// Render returns the header for opts, ending with a newline.
func Render(opts Options) string {
	if !opts.Unicode {
		opts.Color = NoColor // conservative: dumb or non-UTF-8 terminals
	}
	mono := opts.Color == NoColor
	var c canvas
	switch Choose(opts) {
	case Wide:
		owl := owlLarge.canvas(mono)
		c = newCanvas(owlLarge.height(), WideWidth)
		c.blit(owl, 0, margin)
		x := margin + owlLarge.width() + wideGap
		c.blit(wordmarkArt, 1, x)
		c.text(Subtitle, len(wordmarkArt)+2, x+1, nil)
	case Compact:
		owl := owlSmall.canvas(mono)
		c = newCanvas(owlSmall.height(), CompactWidth)
		c.blit(owl, 0, margin)
		x := margin + owlSmall.width() + compactGap
		for i := range len(word) {
			col := gradient(float64(i) / float64(len(word)-1))
			c[1][x+i] = cell{rune(word[i]), col, true}
		}
		c.text(compactLines[0], 3, x, nil)
		c.text(compactLines[1], 4, x, nil)
	default:
		return plain(opts)
	}
	return c.encode(opts.Color)
}

// plain wraps the title and subtitle to the width without decoration.
func plain(opts Options) string {
	var b strings.Builder
	title := word
	if opts.Color != NoColor {
		title = sgr(lavender, opts.Color) + word + reset
	}
	b.WriteString(title + "\n")
	line := ""
	for _, w := range strings.Fields(Subtitle) {
		if line != "" && opts.Width > 0 && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > opts.Width {
			b.WriteString(line + "\n")
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	b.WriteString(line + "\n")
	return b.String()
}

const reset = "\x1b[0m"

func (c canvas) encode(mode ColorMode) string {
	var b strings.Builder
	for _, line := range c {
		end := len(line)
		for end > 0 && line[end-1].r == ' ' {
			end--
		}
		active := ""
		for _, v := range line[:end] {
			want := ""
			if v.colored && mode != NoColor {
				want = sgr(v.c, mode)
			}
			if want != active {
				if want == "" {
					b.WriteString(reset)
				} else {
					b.WriteString(want)
				}
				active = want
			}
			b.WriteRune(v.r)
		}
		if active != "" {
			b.WriteString(reset)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func sgr(c rgb, mode ColorMode) string {
	switch mode {
	case TrueColor:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	case Color256:
		return fmt.Sprintf("\x1b[38;5;%dm", c.c256)
	case Basic16:
		return fmt.Sprintf("\x1b[%dm", c.basic)
	}
	return ""
}

// xterm256 returns the nearest index in the 6x6x6 cube or grayscale ramp.
func xterm256(c rgb) int {
	levels := [6]int{0, 95, 135, 175, 215, 255}
	nearest := func(v uint8) int {
		best := 0
		for i, l := range levels {
			if abs(int(v)-l) < abs(int(v)-levels[best]) {
				best = i
			}
		}
		return best
	}
	r, g, b := nearest(c.r), nearest(c.g), nearest(c.b)
	cube := 16 + 36*r + 6*g + b
	cubeDist := dist(c, levels[r], levels[g], levels[b])
	gray := (int(c.r) + int(c.g) + int(c.b)) / 3
	grayIndex := min(max((gray-8+5)/10, 0), 23)
	gv := 8 + 10*grayIndex
	if dist(c, gv, gv, gv) < cubeDist {
		return 232 + grayIndex
	}
	return cube
}

func dist(c rgb, r, g, b int) int {
	dr, dg, db := int(c.r)-r, int(c.g)-g, int(c.b)-b
	return dr*dr + dg*dg + db*db
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// VisibleWidth returns the terminal columns s occupies, ignoring SGR escape
// sequences. It assumes one column per rune, which holds for every glyph the
// banner emits.
func VisibleWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}
