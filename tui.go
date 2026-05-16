package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/comnik/autoprobe/internal/provider"
)

// How long a transient visual flash (DISTILL badge, library "changed"
// pulse) holds after the triggering event before fading back to the
// resting style.
const flashDuration = 2 * time.Second

type tuiState int

const (
	stateInit tuiState = iota
	stateRunning
	stateDone
	stateError
)

type tuiModel struct {
	agent   *Agent
	ctx     context.Context
	state   tuiState
	err     error
	width   int
	height  int
	ready   bool
	phaseCh <-chan struct{}

	// pulsedAtIter tracks the most recent iteration at which the library
	// bar flashed each program's segment, so the pulse is single-tick:
	// the segment goes bright the first time we observe a change for an
	// iteration and settles back to its resting style on the next
	// refresh. The agent's snapshot would otherwise re-assert
	// ChangedThisIter on every tick within the same iteration.
	pulsedAtIter map[string]int
}

type primedMsg struct{ err error }

type stepMsg struct {
	done bool
	err  error
}

type tickMsg struct{}
type phaseMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func runTUI(ctx context.Context, agent *Agent) error {
	m := tuiModel{
		agent:        agent,
		ctx:          ctx,
		state:        stateInit,
		pulsedAtIter: map[string]int{},
		phaseCh:      agent.SubscribePhaseChanges(),
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			err := m.agent.Prime(m.ctx)
			return primedMsg{err: err}
		},
		tickCmd(),
		waitPhase(m.phaseCh),
	)
}

func waitPhase(ch <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		return phaseMsg{}
	}
}

func (m tuiModel) step() tea.Cmd {
	return func() tea.Msg {
		_, done, err := m.agent.Step(m.ctx)
		return stepMsg{done: done, err: err}
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Wipe any stale cells the standard renderer would otherwise
		// leave behind when the new render's line count shrinks (the
		// dashboard's height varies with assistant-message wrap).
		return m, tea.ClearScreen

	case primedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateError
			return m, nil
		}
		m.state = stateRunning
		return m, m.step()

	case tickMsg:
		return m, tickCmd()

	case phaseMsg:
		return m, waitPhase(m.phaseCh)

	case stepMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateError
			return m, nil
		}
		if msg.done {
			m.state = stateDone
			return m, nil
		}
		return m, m.step()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter", " ":
			if m.state == stateDone || m.state == stateError {
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m tuiModel) contentWidth() int {
	w := m.width
	if w < 20 {
		w = 20
	}
	return w
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing…"
	}
	cw := m.contentWidth()

	bars := m.renderBars(cw)
	sep := strings.Repeat("─", cw)
	sections := []string{
		m.renderHeader(cw),
		"",
		m.renderPhase(cw),
		sep,
		m.renderTokens(cw),
		bars,
		sep,
		m.renderAssistant(cw),
	}
	return strings.Join(sections, "\n")
}

// renderBars renders the budget, drag, and library bars stacked, sharing
// a common annotation slot so all three bars line up to the same width.
func (m tuiModel) renderBars(width int) string {
	budget := m.budgetState()
	drag := m.dragState()
	lib := m.libraryState()

	slot := lipgloss.Width(budget.annotation)
	if w := lipgloss.Width(drag.annotation); w > slot {
		slot = w
	}
	if w := lipgloss.Width(lib.annotation); w > slot {
		slot = w
	}

	rows := []string{
		m.renderBar(width, "drag", drag.pct, drag.fill, drag.annotation, slot, -1),
		m.renderBar(width, "budget", budget.pct, budget.fill, budget.annotation, slot, 0.8),
		m.renderLibraryBar(width, lib, slot),
	}
	return strings.Join(rows, "\n")
}

type barState struct {
	pct        float64
	fill       lipgloss.Style
	annotation string
}

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	infoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	errStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	errBannerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("203"))
	footerKey      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	footerLabel    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	asstStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	pipOnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("84"))
	pipOffStyle  = mutedStyle
	pipLabel     = infoStyle
	barFillGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("84"))
	barFillAmber = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	barFillRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	barEmpty     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	flashStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))

	// Library segments alternate through this palette so adjacent
	// active programs are visually distinguishable even when their
	// segments are all "active, unchanged". Hand-picked greens with
	// enough hop between neighbors to read as separate bands.
	librarySegmentPalette = []lipgloss.Style{
		lipgloss.NewStyle().Foreground(lipgloss.Color("84")),
		lipgloss.NewStyle().Foreground(lipgloss.Color("35")),
		lipgloss.NewStyle().Foreground(lipgloss.Color("78")),
		lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
	}
)

func (m tuiModel) renderHeader(width int) string {
	title := titleStyle.Render("autoprobe v" + Version)
	model := m.agent.Provider().DefaultModel()
	idle := ""
	if polls, since, active := m.agent.IdleStatus(); active {
		idle = fmt.Sprintf("   idle: %d polls, %s", polls, since.Truncate(time.Second))
	}
	left := title + "   " + infoStyle.Render(model+idle)
	right := footerKey.Render("q") + footerLabel.Render(" quit")
	if m.state == stateDone || m.state == stateError {
		right = footerKey.Render("enter") + footerLabel.Render(" exit")
	}
	pad := width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func (m tuiModel) renderPhase(width int) string {
	cur := m.agent.Phase()
	type pip struct {
		val   uint32
		label string
	}
	pips := []pip{
		{PhaseRunPrograms, "running programs"},
		{PhaseInference, "inference"},
		{PhaseTools, "tools"},
		{PhaseIdle, "idle"},
	}
	var parts []string
	for _, p := range pips {
		mark := "○"
		style := pipOffStyle
		if p.val == cur {
			mark = "●"
			style = pipOnStyle
		}
		parts = append(parts, style.Render(mark)+" "+pipLabel.Render(p.label))
	}
	left := strings.Join(parts, "   ")
	right := infoStyle.Render(fmt.Sprintf("cycles: %d", m.agent.ToolCycles()))
	pad := width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func (m tuiModel) renderTokens(width int) string {
	in, out := m.agent.TotalTokens()
	tokensCell := fmt.Sprintf("tokens   %s in  /  %s out", humanInt(in), humanInt(out))
	costCell := "est. cost   —"
	if p, ok := lookupPrice(m.agent.Provider().Name(), m.agent.Provider().DefaultModel()); ok {
		costCell = fmt.Sprintf("est. cost   $%.4f", estimateCost(p, in, out))
	}
	// Right-align the cost cell at the panel width.
	pad := width - lipgloss.Width(tokensCell) - lipgloss.Width(costCell)
	if pad < 1 {
		pad = 1
	}
	return infoStyle.Render(tokensCell) + strings.Repeat(" ", pad) + infoStyle.Render(costCell)
}

func (m tuiModel) budgetState() barState {
	used := m.agent.LastProgramTokens()
	budget := m.agent.ContextBudget()
	pct := 0.0
	if budget > 0 {
		pct = float64(used) / float64(budget)
	}
	style := barFillGreen
	if pct >= 1.0 {
		style = barFillRed
	} else if pct >= 0.8 {
		style = barFillAmber
	}
	return barState{
		pct:        pct,
		fill:       style,
		annotation: fmt.Sprintf("%d%%   %s / %s tok", clampPct(pct), humanInt(used), humanInt(budget)),
	}
}

func (m tuiModel) dragState() barState {
	drag, valid := m.agent.LastDrag()
	flashing := false
	if t := m.agent.LastDistillFiredAt(); !t.IsZero() && time.Since(t) <= flashDuration {
		flashing = true
	}
	if !valid {
		annotation := "—"
		if flashing {
			annotation = flashStyle.Render("DISTILL") + "  " + annotation
		}
		return barState{pct: 0, fill: barEmpty, annotation: annotation}
	}
	pct := float64(drag) / float64(distillThresholdTokens)
	style := barFillGreen
	if pct >= 1.0 {
		style = barFillRed
	} else if pct >= 0.8 {
		style = barFillAmber
	}
	annotation := fmt.Sprintf("%d%%   %s / %s tok", clampPct(pct), humanInt(drag), humanInt(distillThresholdTokens))
	if flashing {
		annotation = flashStyle.Render("DISTILL") + "  " + annotation
	}
	return barState{pct: pct, fill: style, annotation: annotation}
}

// renderBar draws "<label>  ████░░░░ <annotation>" sized to fit width.
// The annotation is right-padded to annotationSlot cells so multiple bars
// stacked together share the same bar width regardless of their per-row
// annotation length. shoulderAt, when in [0,1], draws a small tick at
// that fraction inside the empty portion to mark a threshold (e.g. the
// 80% active/exploration split on the budget bar). Negative suppresses
// the tick.
func (m tuiModel) renderBar(width int, label string, pct float64, fill lipgloss.Style, annotation string, annotationSlot int, shoulderAt float64) string {
	labelCell := infoStyle.Render(fmt.Sprintf("%-7s ", label))
	labelWidth := lipgloss.Width(labelCell)

	annoVisible := lipgloss.Width(annotation)
	if annotationSlot < annoVisible {
		annotationSlot = annoVisible
	}
	pad := annotationSlot - annoVisible
	paddedAnnotation := " " + strings.Repeat(" ", pad) + annotation
	annoWidth := 1 + annotationSlot

	barWidth := width - labelWidth - annoWidth
	if barWidth < 8 {
		barWidth = 8
	}

	if pct < 0 {
		pct = 0
	}
	displayPct := pct
	if displayPct > 1 {
		displayPct = 1
	}
	filled := int(float64(barWidth) * displayPct)
	if filled > barWidth {
		filled = barWidth
	}
	if pct > 0 && filled == 0 {
		filled = 1
	}

	emptyCells := barWidth - filled
	emptyRunes := make([]rune, emptyCells)
	for i := range emptyRunes {
		emptyRunes[i] = '░'
	}
	if shoulderAt >= 0 && shoulderAt < 1 {
		shoulderIdx := int(float64(barWidth) * shoulderAt)
		if shoulderIdx >= filled && shoulderIdx-filled < emptyCells {
			emptyRunes[shoulderIdx-filled] = '┊'
		}
	}
	bar := fill.Render(strings.Repeat("█", filled)) + barEmpty.Render(string(emptyRunes))
	return labelCell + bar + infoStyle.Render(paddedAnnotation)
}

// libraryStateData carries enough to render the library bar after the
// shared annotation slot has been computed. The state's annotation
// participates in slot sizing; placeholder is the message that replaces
// the bar when there are no programs / no tokens to render.
type libraryStateData struct {
	snap        []ProgramSnapshot
	annotation  string
	placeholder string
	totalTokens int
}

func (m tuiModel) libraryState() libraryStateData {
	snap := m.agent.LastProgramSnapshot()
	if len(snap) == 0 {
		return libraryStateData{snap: nil, annotation: "—", placeholder: "(no programs yet)"}
	}
	total := 0
	for _, s := range snap {
		total += s.RenderedTokens
	}
	annotation := fmt.Sprintf("%d / %d changed", countChanged(snap), len(snap))
	placeholder := ""
	if total <= 0 {
		placeholder = "(0 tokens)"
	}
	return libraryStateData{snap: snap, annotation: annotation, placeholder: placeholder, totalTokens: total}
}

func (m tuiModel) renderLibraryBar(width int, lib libraryStateData, annotationSlot int) string {
	labelCell := infoStyle.Render(fmt.Sprintf("%-7s ", "library"))
	labelWidth := lipgloss.Width(labelCell)

	annoVisible := lipgloss.Width(lib.annotation)
	if annotationSlot < annoVisible {
		annotationSlot = annoVisible
	}
	pad := annotationSlot - annoVisible
	paddedAnnotation := " " + strings.Repeat(" ", pad) + lib.annotation
	annoWidth := 1 + annotationSlot

	barWidth := width - labelWidth - annoWidth
	if barWidth < 8 {
		barWidth = 8
	}

	if lib.placeholder != "" || len(lib.snap) == 0 {
		body := lib.placeholder
		if body == "" {
			body = "—"
		}
		return labelCell + mutedStyle.Render(body) + infoStyle.Render(paddedAnnotation)
	}

	iter := m.agent.Iteration()
	cells := allocateCells(lib.snap, barWidth)
	var b strings.Builder
	activeIdx := 0
	for i, s := range lib.snap {
		w := cells[i]
		if w <= 0 {
			continue
		}
		pulsing := false
		if s.ChangedThisIter && m.pulsedAtIter[s.Name] != iter {
			m.pulsedAtIter[s.Name] = iter
			pulsing = true
		}
		palette := librarySegmentPalette[activeIdx%len(librarySegmentPalette)]
		b.WriteString(segmentRender(s, w, pulsing, palette))
		if s.Active && !s.Dropped {
			activeIdx++
		}
	}
	return labelCell + b.String() + infoStyle.Render(paddedAnnotation)
}

func segmentRender(s ProgramSnapshot, width int, pulsing bool, activePalette lipgloss.Style) string {
	var style lipgloss.Style
	ch := "█"
	switch {
	case s.Dropped:
		style = barFillRed
		ch = "▒"
	case !s.Active && s.IncludedInContext:
		style = mutedStyle
		ch = "▒"
	case !s.Active:
		style = mutedStyle
		ch = "░"
	case pulsing:
		style = flashStyle
		ch = "█"
	default:
		style = activePalette
		ch = "█"
	}
	return style.Render(strings.Repeat(ch, width))
}

func allocateCells(snap []ProgramSnapshot, width int) []int {
	cells := make([]int, len(snap))
	total := 0
	for _, s := range snap {
		total += s.RenderedTokens
	}
	if total <= 0 {
		return cells
	}
	remaining := width
	for i, s := range snap {
		w := int(float64(s.RenderedTokens) / float64(total) * float64(width))
		cells[i] = w
		remaining -= w
	}
	// Distribute leftover cells to the largest programs first so rounding
	// doesn't always pile onto the leftmost segment.
	for remaining > 0 {
		best, bestVal := -1, -1
		for i, s := range snap {
			if s.RenderedTokens > bestVal {
				best = i
				bestVal = s.RenderedTokens
			}
		}
		if best < 0 {
			break
		}
		cells[best]++
		remaining--
	}
	return cells
}

func countChanged(snap []ProgramSnapshot) int {
	n := 0
	for _, s := range snap {
		if s.ChangedThisIter {
			n++
		}
	}
	return n
}

func (m tuiModel) renderAssistant(width int) string {
	if m.state == stateError && m.err != nil {
		banner := errBannerStyle.Render(" ERROR ")
		body := errStyle.Width(width).Render(m.err.Error())
		return banner + "\n\n" + body
	}
	text := latestAssistantText(m.agent.Conversation())
	if text == "" {
		return mutedStyle.Render("(waiting for first response)")
	}
	return asstStyle.Width(width).Render(text)
}

func latestAssistantText(conversation []provider.Message) string {
	for i := len(conversation) - 1; i >= 0; i-- {
		am, ok := conversation[i].(provider.AssistantMessage)
		if !ok {
			continue
		}
		for j := len(am.Content) - 1; j >= 0; j-- {
			if tc, ok := am.Content[j].(provider.TextContent); ok && strings.TrimSpace(tc.Text) != "" {
				return tc.Text
			}
		}
	}
	return ""
}


// humanInt formats a token count compactly: integers below 1K, one decimal
// in the 1K–10K range (e.g. "1.2K"), and rounded integers thereafter (e.g.
// "131K", "1.5M"). Uses base-1000 scaling so the displayed numbers match
// the canonical "131K context window" rather than the 1024-based 128K.
func humanInt(n int) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dK", (n+500)/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	}
}

func clampPct(p float64) int {
	if p < 0 {
		return 0
	}
	if p > 9.99 {
		return 999
	}
	return int(p*100 + 0.5)
}

