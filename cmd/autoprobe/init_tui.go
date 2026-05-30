package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// providerChoice is one row in the provider picker.
type providerChoice struct {
	id    string // "anthropic" | "openai" | "google" | "grok"
	label string
	hint  string
}

var providerChoices = []providerChoice{
	{id: "anthropic", label: "Anthropic", hint: "Claude (uses ANTHROPIC_API_KEY)"},
	{id: "openai", label: "OpenAI", hint: "GPT / Codex (uses OPENAI_API_KEY)"},
	{id: "google", label: "Google", hint: "Gemini (uses GEMINI_API_KEY or GOOGLE_API_KEY)"},
	{id: "grok", label: "Grok (xAI)", hint: "Grok (uses XAI_API_KEY)"},
}

// modelChoice is one row in the model picker.
type modelChoice struct {
	id    string // empty means "use provider default"
	label string
	hint  string
}

// suggestedModels lists a few canonical model ids per provider plus a
// "(provider default)" entry that writes an empty model. "Custom..." swaps
// the picker into a text-input prompt for ids not on the list.
func suggestedModels(provider string) []modelChoice {
	switch provider {
	case "anthropic":
		return []modelChoice{
			{id: "", label: "(provider default)", hint: "let autoprobe pick"},
			{id: "claude-opus-4-7", label: "Claude Opus 4.7", hint: "most capable"},
			{id: "claude-sonnet-4-6", label: "Claude Sonnet 4.6"},
			{id: "claude-haiku-4-5", label: "Claude Haiku 4.5", hint: "fastest"},
		}
	case "openai":
		return []modelChoice{
			{id: "", label: "(provider default)", hint: "let autoprobe pick"},
			{id: "gpt-5.3-codex", label: "gpt-5.3-codex", hint: "code-focused reasoning"},
			{id: "gpt-5.5", label: "gpt-5.5", hint: "most capable"},
			{id: "gpt-5.4", label: "gpt-5.4"},
			{id: "gpt-5.4-mini", label: "gpt-5.4-mini", hint: "fastest"},
		}
	case "google":
		return []modelChoice{
			{id: "", label: "(provider default)", hint: "let autoprobe pick"},
			{id: "gemini-2.5-pro", label: "Gemini 2.5 Pro"},
			{id: "gemini-2.5-flash", label: "Gemini 2.5 Flash", hint: "fastest"},
		}
	case "grok":
		return []modelChoice{
			{id: "", label: "(provider default)", hint: "let autoprobe pick"},
			{id: "grok-4.3", label: "grok-4.3", hint: "most capable"},
			{id: "grok-build-0.1", label: "grok-build-0.1", hint: "code-focused, 256k ctx"},
		}
	}
	return []modelChoice{{id: "", label: "(provider default)"}}
}

type pickerStep int

const (
	stepProvider pickerStep = iota
	stepModel
	stepModelCustom
	stepDone
)

type pickerModel struct {
	step       pickerStep
	skipProvider bool
	skipModel    bool

	// Provider state.
	providers []providerChoice
	provIdx   int

	// Model state.
	models      []modelChoice
	modelIdx    int
	customInput textinput.Model

	// Final answers.
	provider string
	model    string

	cancelled bool
	width     int
}

func newPickerModel(initial Config, skipProvider, skipModel bool) pickerModel {
	ti := textinput.New()
	ti.Placeholder = "model id (e.g. claude-opus-4-7)"
	ti.CharLimit = 128
	ti.Prompt = "› "

	m := pickerModel{
		step:         stepProvider,
		skipProvider: skipProvider,
		skipModel:    skipModel,
		providers:    providerChoices,
		customInput:  ti,
		provider:     initial.Provider,
		model:        initial.Model,
	}
	if skipProvider {
		// Provider already settled (from --provider or existing config when user
		// only wants to repick the model). Skip the provider screen.
		m.step = stepModel
	}
	for i, p := range providerChoices {
		if p.id == initial.Provider {
			m.provIdx = i
			break
		}
	}
	m.refreshModels(initial.Model)
	return m
}

// refreshModels recomputes the model list for the current provider and tries
// to pre-select selectModel by id (or the "(provider default)" entry if
// selectModel is empty).
func (m *pickerModel) refreshModels(selectModel string) {
	prov := m.providers[m.provIdx].id
	m.models = suggestedModels(prov)
	m.models = append(m.models, modelChoice{id: "__custom__", label: "Custom…"})
	m.modelIdx = 0
	for i, mm := range m.models {
		if mm.id == selectModel {
			m.modelIdx = i
			break
		}
	}
}

func (m pickerModel) Init() tea.Cmd { return textinput.Blink }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch m.step {
		case stepProvider:
			return m.updateProvider(msg)
		case stepModel:
			return m.updateModel(msg)
		case stepModelCustom:
			return m.updateModelCustom(msg)
		}
	}
	return m, nil
}

func (m pickerModel) updateProvider(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q", "esc":
		m.cancelled = true
		return m, tea.Quit
	case "up", "k":
		if m.provIdx > 0 {
			m.provIdx--
		}
	case "down", "j":
		if m.provIdx < len(m.providers)-1 {
			m.provIdx++
		}
	case "enter":
		m.provider = m.providers[m.provIdx].id
		if m.skipModel {
			m.step = stepDone
			return m, tea.Quit
		}
		m.refreshModels(m.model)
		m.step = stepModel
	}
	return m, nil
}

func (m pickerModel) updateModel(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		if m.skipProvider {
			m.cancelled = true
			return m, tea.Quit
		}
		m.step = stepProvider
	case "up", "k":
		if m.modelIdx > 0 {
			m.modelIdx--
		}
	case "down", "j":
		if m.modelIdx < len(m.models)-1 {
			m.modelIdx++
		}
	case "enter":
		choice := m.models[m.modelIdx]
		if choice.id == "__custom__" {
			m.customInput.SetValue(m.model)
			m.customInput.Focus()
			m.step = stepModelCustom
			return m, textinput.Blink
		}
		m.model = choice.id
		m.step = stepDone
		return m, tea.Quit
	}
	return m, nil
}

func (m pickerModel) updateModelCustom(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		m.step = stepModel
		m.customInput.Blur()
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.customInput.Value())
		if v == "" {
			// Treat empty as "(provider default)".
			m.model = ""
		} else {
			m.model = v
		}
		m.step = stepDone
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.customInput, cmd = m.customInput.Update(msg)
	return m, cmd
}

var (
	pickerTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	pickerHintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	pickerSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	pickerCursorStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	pickerLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	pickerKeyStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	pickerFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func (m pickerModel) View() string {
	var b strings.Builder
	b.WriteString(pickerTitleStyle.Render("autoprobe v" + Version + " · init"))
	b.WriteString("\n\n")

	switch m.step {
	case stepProvider:
		b.WriteString(pickerLabelStyle.Render("Select provider:"))
		b.WriteString("\n\n")
		for i, p := range m.providers {
			b.WriteString(renderRow(i == m.provIdx, p.label, p.hint))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(pickerFooter("↑/↓ select", "enter confirm", "q quit"))
	case stepModel:
		b.WriteString(pickerLabelStyle.Render(fmt.Sprintf("Select %s model:", m.providers[m.provIdx].label)))
		b.WriteString("\n\n")
		for i, mm := range m.models {
			b.WriteString(renderRow(i == m.modelIdx, mm.label, mm.hint))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		footer := []string{"↑/↓ select", "enter confirm", "q quit"}
		if !m.skipProvider {
			footer = append([]string{"esc back"}, footer...)
		}
		b.WriteString(pickerFooter(footer...))
	case stepModelCustom:
		b.WriteString(pickerLabelStyle.Render("Enter model id:"))
		b.WriteString("\n\n")
		b.WriteString(m.customInput.View())
		b.WriteString("\n\n")
		b.WriteString(pickerFooter("enter confirm", "esc back", "ctrl+c quit"))
	case stepDone:
		// Should already have quit; render nothing.
	}

	return b.String()
}

func renderRow(selected bool, label, hint string) string {
	cursor := "  "
	render := pickerLabelStyle.Render
	if selected {
		cursor = pickerCursorStyle.Render("▶ ")
		render = pickerSelectedStyle.Render
	}
	row := cursor + render(label)
	if hint != "" {
		row += "  " + pickerHintStyle.Render(hint)
	}
	return row
}

func pickerFooter(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// Bold the key (everything before the first space).
		idx := strings.IndexByte(p, ' ')
		if idx < 0 {
			out = append(out, pickerKeyStyle.Render(p))
			continue
		}
		out = append(out, pickerKeyStyle.Render(p[:idx])+pickerFooterStyle.Render(p[idx:]))
	}
	return pickerFooterStyle.Render(strings.Join(out, pickerFooterStyle.Render("  •  ")))
}

// runInitPicker shows an interactive provider/model selector. Pass an
// existing Config (zero-valued is fine) to pre-select choices on update.
// skipProvider/skipModel let the caller skip a step when that field was
// already supplied via a flag.
func runInitPicker(initial Config, skipProvider, skipModel bool) (Config, error) {
	if skipProvider && skipModel {
		return initial, nil
	}
	m := newPickerModel(initial, skipProvider, skipModel)
	out, err := tea.NewProgram(m).Run()
	if err != nil {
		return Config{}, err
	}
	final := out.(pickerModel)
	if final.cancelled {
		return Config{}, fmt.Errorf("init cancelled")
	}
	return Config{Provider: final.provider, Model: final.model}, nil
}
