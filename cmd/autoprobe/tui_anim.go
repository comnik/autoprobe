package main

import "github.com/charmbracelet/lipgloss"

// Sonar-themed turn-kind indicators for the autoprobe phase strip.
//
// Each animation is a fixed-size block of rows: `frames[tick][row]`,
// where every row has identical monospace width. The TUI renders the
// block to the left of a horizontal pip strip on its middle row; both
// indicators are the same size so the layout never shifts when the
// turn kind flips.
//
// concentricPings → modeling turns (reflective, periodic).
// wavefront       → work turns (active forward push).

const (
	animRows = 3
	animCols = 21
)

var (
	// workAnimStyle is a calm cyan — the wavefront reads as ongoing
	// signal propagation without competing with the bars below for
	// attention.
	workAnimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

	// modelingAnimStyle matches the resting modeling color so the
	// animation and any modeling-specific UI read as one indicator.
	modelingAnimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
)

// concentricPings: an impulse at the center expands outward through
// rings of increasing radius, then fades. Six frames cycle through one
// complete ping-and-fade.
var concentricPings = [][]string{
	{
		"                     ",
		"          •          ",
		"                     ",
	},
	{
		"         ···         ",
		"         ·•·         ",
		"         ···         ",
	},
	{
		"        · · ·        ",
		"        · • ·        ",
		"        · · ·        ",
	},
	{
		"       · · · ·       ",
		"       ·  •  ·       ",
		"       · · · ·       ",
	},
	{
		"      · · · · ·      ",
		"      ·   •   ·      ",
		"      · · · · ·      ",
	},
	{
		"                     ",
		"                     ",
		"                     ",
	},
}

// wavefront: a band of block characters whose amplitude ripples from
// left to right, like a probe pulse propagating through a medium. Six
// frames slide the crest one column at a time; the crest wraps from
// right back to left so the loop is seamless.
//
// Uses unicode block elements ▁▂▃▄▅▆▇█ so brightness reads as height.
var wavefront = [][]string{
	{
		"▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁",
		"█▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁▁▁▁",
		"▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁",
	},
	{
		"▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁▁",
		"▅▆▇█▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁",
		"▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁▁▁▁",
	},
	{
		"▁▂▃▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁",
		"▁▂▃▄▅▆▇█▇▆▅▄▃▂▁▁▁▁▁▁▁",
		"▁▂▃▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁▁▁▁",
	},
	{
		"▁▁▁▁▂▃▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁",
		"▁▁▁▁▂▃▄▅▆▇█▇▆▅▄▃▂▁▁▁▁",
		"▁▁▁▁▂▃▄▅▆▇▆▅▄▃▂▁▁▁▁▁▁",
	},
	{
		"▁▁▁▁▁▁▁▁▂▃▄▅▆▇▆▅▄▃▂▁▁",
		"▁▁▁▁▁▁▁▁▂▃▄▅▆▇█▇▆▅▄▃▂",
		"▁▁▁▁▁▁▁▁▂▃▄▅▆▇▆▅▄▃▂▁▁",
	},
	{
		"▁▁▁▁▁▁▁▁▁▁▁▁▂▃▄▅▆▇▆▅▄",
		"▁▁▁▁▁▁▁▁▁▁▁▁▂▃▄▅▆▇█▇▆",
		"▁▁▁▁▁▁▁▁▁▁▁▁▂▃▄▅▆▇▆▅▄",
	},
}
