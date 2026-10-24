package banner

// Foreground-only colors leave the user's terminal background intact.
var owlPalette = map[byte]rgb{
	'l': {0xB5, 0x8D, 0xDC, 140, 95}, // violet rim
	'h': {0x70, 0x41, 0x83, 96, 35},  // plum face
	'b': {0x86, 0x53, 0x98, 97, 35},  // chest
	'w': {0x50, 0x28, 0x64, 53, 35},  // folded wings
	'e': {0xFF, 0xBD, 0x61, 215, 93}, // golden eye discs
	'i': {0xDC, 0x85, 0x32, 172, 33}, // amber iris
	'p': {0x35, 0x20, 0x43, 235, 90}, // pupils
	't': {0xFF, 0xEE, 0xD5, 230, 97}, // eye highlights
	'k': {0xF0, 0x6A, 0x7D, 203, 91}, // coral beak
	'f': {0xDA, 0xA1, 0x58, 179, 93}, // chest feathers
}

// Eye discs become cut-outs in monochrome, preserving the pupils.
const monoHollow = "eik"

// Each cell is roughly twice as tall as it is wide; half-blocks soften
// the silhouette without adding background colors or extra terminal rows.
var owlLarge = art{
	glyphs: []string{
		"       ▚▖  ▗▞       ",
		"    ▄▄▄▄████▄▄▄▄    ",
		"  █▄████▄██▄████▄█  ",
		" ████▝███████▝█████ ",
		" ████████▄▄████████ ",
		"  █▀████▀▀▀▀████▀█  ",
		" █▟███▙██████▟███▙█ ",
		"█▓██▙██▚▞▚▞▚▞██▟██▓█",
		"█▓▓█▙███▚▞▚▞███▟█▓▓█",
		" █▀▓▙████▚▞████▟▓▀█ ",
		"    ▀▀████████▀▀    ",
	},
	classes: []string{
		"       ll  ll       ",
		"    llllhhhhllll    ",
		"  leeeeeehheeeeeel  ",
		" lheitpiehheitpiehl ",
		" lheippiekkeippiehl ",
		"  leeeeeekkeeeeeel  ",
		" lwwwwbhhhhhbwwwwwl ",
		"lwwwwbbffffffbbwwwwl",
		"lwwwwbbbffffbbbwwwwl",
		" lwwwbbbbffbbbbwwwl ",
		"    bbhhbbbbhhbb    ",
	},
}

var owlSmall = art{
	glyphs: []string{
		"   ▚▄▄█▄▄▞   ",
		" ▄█████████▄ ",
		" ██▝████▝███ ",
		" █▀██▀█▀██▀█ ",
		"▟▓█████████▓▙",
		" ▓▙██▚▞███▟▓ ",
		"  ▀▀█████▀▀  ",
	},
	classes: []string{
		"   llhhhll   ",
		" lheeeheeehl ",
		" letpeletpel ",
		" heeeekeeeeh ",
		"lwwbhhhhhbwwl",
		" wwbbffbbbww ",
		"  wwbbbbbww  ",
	},
}
