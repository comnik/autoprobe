package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	agent       *Agent
	ctx         context.Context
	state       tuiState
	stepThrough bool
	err         error
	width       int
	height      int
	viewport    viewport.Model
	ready       bool
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
		headerH := 2
		footerH := 2
		bodyH := msg.Height - headerH - footerH
		if bodyH < 1 {
			bodyH = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, bodyH)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = bodyH
		}
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
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
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

func (m *tuiModel) refreshContent() {
	if !m.ready {
		return
	}
	m.viewport.SetContent(renderConversation(m.agent.Conversation(), m.width))
	m.viewport.GotoBottom()
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing…"
	}
	return strings.Join([]string{
		m.renderHeader(),
		m.viewport.View(),
		m.renderFooter(),
	}, "\n")
}

var (
	headerTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	headerInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	headerErrStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	footerStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	footerKeyStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))

	roleUserStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	roleAsstStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	textBodyStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	toolUseStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("84"))
	toolNameStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("84"))
	toolOkStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("251"))
	toolErrStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	thinkingStyle  = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("141"))
	separatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

func (m tuiModel) renderHeader() string {
	title := headerTitleStyle.Render("hopper")
	info := fmt.Sprintf("iteration %d  •  %s", m.agent.Iteration(), m.stateLabel())
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
		footerKeyStyle.Render("↑/↓")+footerStyle.Render(" scroll"),
		footerKeyStyle.Render("q")+footerStyle.Render(" quit"),
	)
	return footerStyle.Render(strings.Repeat("─", max(m.width, 1))) + "\n" + strings.Join(parts, footerStyle.Render("  •  "))
}

func renderConversation(conversation []anthropic.MessageParam, width int) string {
	var b strings.Builder
	for i, msg := range conversation {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(separatorStyle.Render(strings.Repeat("─", max(width, 1))))
			b.WriteString("\n\n")
		}
		b.WriteString(renderMessage(msg, width))
	}
	if len(conversation) == 0 {
		return headerInfoStyle.Render("(no messages yet)")
	}
	return b.String()
}

func renderMessage(msg anthropic.MessageParam, width int) string {
	var b strings.Builder
	role := strings.ToUpper(string(msg.Role))
	switch msg.Role {
	case anthropic.MessageParamRoleUser:
		b.WriteString(roleUserStyle.Render(role))
	default:
		b.WriteString(roleAsstStyle.Render(role))
	}
	b.WriteString("\n")
	for i, block := range msg.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(renderBlock(block, width))
	}
	return b.String()
}

func renderBlock(block anthropic.ContentBlockParamUnion, width int) string {
	switch {
	case block.OfText != nil:
		return textBodyStyle.Render(block.OfText.Text)
	case block.OfThinking != nil:
		return thinkingStyle.Render("(thinking) " + block.OfThinking.Thinking)
	case block.OfToolUse != nil:
		input, _ := json.Marshal(block.OfToolUse.Input)
		return toolNameStyle.Render("→ "+block.OfToolUse.Name) + toolUseStyle.Render("("+string(input)+")")
	case block.OfToolResult != nil:
		var result strings.Builder
		for _, c := range block.OfToolResult.Content {
			if c.OfText != nil {
				result.WriteString(c.OfText.Text)
			}
		}
		text := result.String()
		isErr := block.OfToolResult.IsError.Or(false)
		label := "← result"
		style := toolOkStyle
		if isErr {
			label = "← error"
			style = toolErrStyle
		}
		return style.Render(label+":\n") + style.Render(indent(text, "  "))
	}
	return headerInfoStyle.Render("(unsupported block)")
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
