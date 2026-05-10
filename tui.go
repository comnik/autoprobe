package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	maxMsgHeight  = 20
	maxLineLength = 92
)

type tuiState int

const (
	stateInit tuiState = iota
	stateReady
	stateRunning
	stateDone
	stateError
)

type tuiModel struct {
	agent        *Agent
	ctx          context.Context
	state        tuiState
	stepThrough  bool
	err          error
	width        int
	height       int
	msgViewports []viewport.Model
	activeIdx    int
	outerVp      viewport.Model
	ready        bool
}

type primedMsg struct{ err error }

type stepMsg struct {
	done bool
	err  error
}

func runTUI(ctx context.Context, agent *Agent) error {
	m := tuiModel{
		agent:       agent,
		ctx:         ctx,
		state:       stateInit,
		stepThrough: agent.StepThrough(),
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m tuiModel) Init() tea.Cmd {
	return func() tea.Msg {
		err := m.agent.Prime(m.ctx)
		return primedMsg{err: err}
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
		m.refreshContent()
		return m, nil

	case primedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateError
			m.refreshContent()
			return m, nil
		}
		var cmd tea.Cmd
		m.state, cmd = m.advance()
		m.refreshContent()
		return m, cmd

	case stepMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateError
			m.refreshContent()
			return m, nil
		}
		if msg.done {
			m.state = stateDone
			m.refreshContent()
			return m, nil
		}
		var cmd tea.Cmd
		m.state, cmd = m.advance()
		m.refreshContent()
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "shift+tab":
			m.stepThrough = !m.stepThrough
			if !m.stepThrough && m.state == stateReady {
				m.state = stateRunning
				m.refreshContent()
				return m, m.step()
			}
			m.refreshContent()
			return m, nil
		case "tab":
			if len(m.msgViewports) > 0 {
				m.activeIdx = (m.activeIdx + 1) % len(m.msgViewports)
				m.refreshContent()
				m.scrollOuterToActive()
			}
			return m, nil
		case "enter", " ":
			switch m.state {
			case stateReady:
				m.state = stateRunning
				m.refreshContent()
				return m, m.step()
			case stateDone, stateError:
				return m, tea.Quit
			}
		}
		var cmd tea.Cmd
		m.outerVp, cmd = m.outerVp.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.outerVp, cmd = m.outerVp.Update(msg)
	return m, cmd
}

// advance picks the next state after a successful prime/step. In step-through
// mode we wait for the user; otherwise we keep firing iterations.
func (m tuiModel) advance() (tuiState, tea.Cmd) {
	if m.stepThrough {
		return stateReady, nil
	}
	return stateRunning, m.step()
}

func (m tuiModel) contentWidth() int {
	w := m.width - 2
	if w > maxLineLength {
		w = maxLineLength
	}
	if w < 1 {
		w = 1
	}
	return w
}

// blockEntry is one renderable unit in the conversation: either a content
// piece of an assistant message (text/thinking/toolCall), a piece of a user
// message (text), or a tool result.
type blockEntry struct {
	role Role

	// At most one of these is non-nil.
	userText   *TextContent
	asstText   *TextContent
	thinking   *ThinkingContent
	toolCall   *ToolCall
	toolResult *ToolResultMessage
}

func (e blockEntry) isAssistantText() bool { return e.asstText != nil }
func (e blockEntry) isUserText() bool      { return e.userText != nil }

func collectBlocks(conversation []Message) []blockEntry {
	var entries []blockEntry
	for _, msg := range conversation {
		switch m := msg.(type) {
		case UserMessage:
			for i := range m.Content {
				c := m.Content[i]
				entries = append(entries, blockEntry{role: RoleUser, userText: &c})
			}
		case AssistantMessage:
			for _, c := range m.Content {
				switch c := c.(type) {
				case TextContent:
					t := c
					entries = append(entries, blockEntry{role: RoleAssistant, asstText: &t})
				case ThinkingContent:
					th := c
					entries = append(entries, blockEntry{role: RoleAssistant, thinking: &th})
				case ToolCall:
					tc := c
					entries = append(entries, blockEntry{role: RoleAssistant, toolCall: &tc})
				}
			}
		case ToolResultMessage:
			tr := m
			entries = append(entries, blockEntry{role: RoleToolResult, toolResult: &tr})
		}
	}
	return entries
}

// findLatestAssistantText returns the index of the most recent assistant text
// block in entries, or -1 if none. That block is pinned at the bottom of the
// TUI so the model's most recent narration stays visible.
func findLatestAssistantText(entries []blockEntry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].isAssistantText() {
			return i
		}
	}
	return -1
}

func (m *tuiModel) refreshContent() {
	if !m.ready {
		return
	}
	entries := collectBlocks(m.agent.Conversation())
	cw := m.contentWidth()

	prevLen := len(m.msgViewports)
	for i := prevLen; i < len(entries); i++ {
		m.msgViewports = append(m.msgViewports, viewport.New(cw, 1))
	}

	for i, entry := range entries {
		body := renderBlock(entry, cw)
		h := lipgloss.Height(body)
		// Inactive blocks cap at maxMsgHeight; the active block expands so the
		// user can see every line.
		if i != m.activeIdx && h > maxMsgHeight {
			h = maxMsgHeight
		}
		if h < 1 {
			h = 1
		}
		m.msgViewports[i].Width = cw
		m.msgViewports[i].Height = h
		m.msgViewports[i].SetContent(body)
		m.msgViewports[i].GotoBottom()
	}

	if len(m.msgViewports) > prevLen {
		m.activeIdx = len(m.msgViewports) - 1
	}
	if m.activeIdx >= len(m.msgViewports) {
		m.activeIdx = 0
	}

	m.refreshOuter()
	if len(m.msgViewports) > prevLen {
		m.outerVp.GotoBottom()
	}
}

// refreshOuter rebuilds the outer viewport's content + dimensions from the
// current per-block viewports, while preserving the user's scroll position.
func (m *tuiModel) refreshOuter() {
	if !m.ready {
		return
	}
	entries := collectBlocks(m.agent.Conversation())
	pinnedIdx := findLatestAssistantText(entries)

	headerH := 1
	footerH := 2
	pinnedH := 0
	if pinnedIdx >= 0 && pinnedIdx < len(m.msgViewports) {
		// separator + header line + viewport body
		pinnedH = 2 + m.msgViewports[pinnedIdx].Height
	}
	outerH := m.height - headerH - footerH - pinnedH
	if outerH < 1 {
		outerH = 1
	}

	cw := m.contentWidth()
	m.outerVp.Width = cw
	m.outerVp.Height = outerH

	yOffset := m.outerVp.YOffset
	content, _ := m.buildOuterContent(entries, pinnedIdx)
	m.outerVp.SetContent(content)
	m.outerVp.YOffset = yOffset
}

// scrollOuterToActive scrolls the outer viewport so that the active block's
// separator becomes the top visible line. No-op when the active block is
// pinned (it's always visible) or when activeIdx is out of range.
func (m *tuiModel) scrollOuterToActive() {
	if !m.ready {
		return
	}
	entries := collectBlocks(m.agent.Conversation())
	pinnedIdx := findLatestAssistantText(entries)
	if m.activeIdx == pinnedIdx {
		return
	}
	_, activeLine := m.buildOuterContent(entries, pinnedIdx)
	if activeLine < 0 {
		return
	}
	maxY := m.outerVp.TotalLineCount() - m.outerVp.Height
	if activeLine > maxY {
		activeLine = maxY
	}
	if activeLine < 0 {
		activeLine = 0
	}
	m.outerVp.YOffset = activeLine
}

// buildOuterContent renders the scrollable region (everything except pinned)
// and returns both the rendered string and the 0-indexed line offset where
// the active block's separator starts (or -1 if the active block is pinned
// or out of range).
func (m tuiModel) buildOuterContent(entries []blockEntry, pinnedIdx int) (string, int) {
	var b strings.Builder
	cw := m.contentWidth()
	first := true
	activeLine := -1
	for i, entry := range entries {
		if i == pinnedIdx {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		if i == m.activeIdx {
			activeLine = lipgloss.Height(b.String()) - 1
		}
		b.WriteString(m.blockSeparator(i, cw))
		b.WriteString("\n")
		b.WriteString(m.renderBlockHeader(i, entry))
		b.WriteString("\n")
		b.WriteString(m.msgViewports[i].View())
	}
	return b.String(), activeLine
}

func (m tuiModel) blockSeparator(i, width int) string {
	style := separatorStyle
	if i == m.activeIdx {
		style = activeSeparatorStyle
	}
	return style.Render(strings.Repeat("─", width))
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing…"
	}
	sections := []string{m.renderHeader()}

	entries := collectBlocks(m.agent.Conversation())
	if len(entries) == 0 {
		sections = append(sections, headerInfoStyle.Render("(no messages yet)"))
	} else {
		sections = append(sections, m.outerVp.View())

		pinnedIdx := findLatestAssistantText(entries)
		if pinnedIdx >= 0 && pinnedIdx < len(m.msgViewports) {
			cw := m.contentWidth()
			sections = append(sections, m.blockSeparator(pinnedIdx, cw))
			sections = append(sections, m.renderBlockHeader(pinnedIdx, entries[pinnedIdx]))
			sections = append(sections, m.msgViewports[pinnedIdx].View())
		}
	}

	sections = append(sections, m.renderFooter())
	return strings.Join(sections, "\n")
}

var (
	headerTitleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	headerInfoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	headerErrStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	footerStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	footerKeyStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	activeSeparatorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	scrollInfoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	roleAsstStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	blockHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	userTextStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	asstTextStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("16"))
	toolUseStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("84"))
	toolNameStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("84"))
	toolOkStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("251"))
	toolErrStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	thinkingStyle    = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("141"))
	separatorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

func (m tuiModel) renderHeader() string {
	title := headerTitleStyle.Render("autoprobe v" + Version)
	info := fmt.Sprintf("iteration %d  •  %s  •  %s", m.agent.Iteration(), m.agent.Provider().Name(), m.stateLabel())
	style := headerInfoStyle
	if m.state == stateError {
		style = headerErrStyle
	}
	return title + "  " + style.Render(info)
}

func (m tuiModel) stateLabel() string {
	switch m.state {
	case stateInit:
		return "priming…"
	case stateReady:
		return "paused (press enter to step)"
	case stateRunning:
		return "running…"
	case stateDone:
		return "done"
	case stateError:
		if m.err != nil {
			return "error: " + m.err.Error()
		}
		return "error"
	}
	return ""
}

func (m tuiModel) renderBlockHeader(i int, entry blockEntry) string {
	var label string
	switch {
	case entry.isAssistantText():
		label = roleAsstStyle.Render("ASSISTANT")
	case entry.isUserText():
		label = blockHeaderStyle.Render(firstLine(entry.userText.Text))
	}

	out := label

	vp := m.msgViewports[i]
	if vp.TotalLineCount() > vp.Height {
		visibleEnd := vp.YOffset + vp.Height
		if visibleEnd > vp.TotalLineCount() {
			visibleEnd = vp.TotalLineCount()
		}
		sep := ""
		if label != "" {
			sep = "  "
		}
		out += sep + scrollInfoStyle.Render(fmt.Sprintf("[%d–%d/%d]", vp.YOffset+1, visibleEnd, vp.TotalLineCount()))
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func skipFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func (m tuiModel) renderFooter() string {
	parts := []string{}
	switch m.state {
	case stateReady:
		parts = append(parts, footerKeyStyle.Render("enter")+footerStyle.Render(" step"))
	case stateRunning:
		parts = append(parts, footerStyle.Render("waiting for model…"))
	case stateDone, stateError:
		parts = append(parts, footerKeyStyle.Render("enter")+footerStyle.Render(" exit"))
	}
	mode := "auto"
	if m.stepThrough {
		mode = "manual"
	}
	parts = append(parts,
		footerKeyStyle.Render("shift+tab")+footerStyle.Render(" "+mode),
		footerKeyStyle.Render("tab")+footerStyle.Render(" focus next"),
		footerKeyStyle.Render("↑/↓")+footerStyle.Render(" scroll"),
		footerKeyStyle.Render("q")+footerStyle.Render(" quit"),
	)
	return footerStyle.Render(strings.Repeat("─", m.contentWidth())) + "\n" + strings.Join(parts, footerStyle.Render("  •  "))
}

func renderBlock(entry blockEntry, width int) string {
	switch {
	case entry.userText != nil:
		// User text blocks promote their first line (front matter like
		// `[program=… exit=…]` or `[YOUR GOAL]`) into the block header.
		return wrapStyled(userTextStyle, skipFirstLine(entry.userText.Text), width)
	case entry.asstText != nil:
		return wrapStyled(asstTextStyle, entry.asstText.Text, width)
	case entry.thinking != nil:
		body := entry.thinking.Thinking
		if entry.thinking.Redacted {
			body = "(redacted)"
		}
		return wrapStyled(thinkingStyle, "(thinking) "+body, width)
	case entry.toolCall != nil:
		input := string(entry.toolCall.Arguments)
		if input == "" {
			input = "{}"
		} else {
			// Pretty up by re-marshalling if it parses; fall back to raw.
			var v any
			if err := json.Unmarshal(entry.toolCall.Arguments, &v); err == nil {
				if b, err := json.Marshal(v); err == nil {
					input = string(b)
				}
			}
		}
		combined := toolNameStyle.Render("→ "+entry.toolCall.Name) + toolUseStyle.Render("("+input+")")
		return lipgloss.NewStyle().Width(width).Render(combined)
	case entry.toolResult != nil:
		text := joinText(entry.toolResult.Content)
		label := "← result:"
		style := toolOkStyle
		if entry.toolResult.IsError {
			label = "← error:"
			style = toolErrStyle
		}
		return wrapStyled(style, label+"\n"+indent(text, "  "), width)
	}
	return headerInfoStyle.Render("(unsupported block)")
}

func wrapStyled(style lipgloss.Style, text string, width int) string {
	if width < 1 {
		width = 1
	}
	return style.Width(width).Render(text)
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
