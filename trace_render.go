package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed viewer/style.css viewer/viewer.js viewer/index.html.tmpl viewer/iter.html.tmpl
var viewerFS embed.FS

// traceTmpls bundles every viewer template in one set so {{template "block"}}
// works across files. ParseFS panics at init time on any template error,
// which is what we want — bad templates are a build-time bug.
var traceTmpls = template.Must(template.New("trace").Funcs(template.FuncMap{
	"formatTime":      func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
	"formatThousands": formatThousands,
	"formatFloat":     func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
	"byteLen":         func(s string) int { return len(s) },
}).ParseFS(viewerFS, "viewer/*.tmpl"))

// staticAssetNames lists the files copied verbatim into the trace dir
// alongside the rendered HTML so the directory is self-contained.
var staticAssetNames = []string{"style.css", "viewer.js"}

// ---------- View types ----------

type indexView struct {
	Header     RunHeader
	Iterations []indexIter
}

type indexIter struct {
	N                   int
	NPadded             string
	WorkIteration       int
	TurnKind            string // "work" (default) or "modeling"
	IsModeling          bool   // convenience for templates
	HTMLFile            string
	RelStart            string
	DurationStr         string
	InputTokens         int
	OutputTokens        int
	StopReason          string
	StopClass           string
	Overflowed          bool
	RevisionPromptFired bool
	IdlePollsBefore     int
}

type iterView struct {
	Header          RunHeader
	Iter            IterationTrace
	TurnKind        string // "work" (default) or "modeling"
	IsModeling      bool
	DurationStr     string
	IdleWaitStr     string
	BudgetPercent   float64
	TotalIterations int
	PrevN, NextN    int
	PrevHref        string
	NextHref        string
	Messages        []viewMessage
	Stats           []viewStat
	HasStats        bool
}

type viewMessage struct {
	Role       string
	RoleLabel  string
	ToolName   string
	ToolCallID string
	IsError    bool
	Anchor     string
	Blocks     []viewBlock
}

type viewBlock struct {
	Kind               string
	Text               string
	Signature          string
	SignaturePreview   string
	ToolName           string
	ToolCallID         string
	ToolAnchor         string
	ProgramName        string
	ProgramTail        string
	ProgramExitNonzero bool
}

type viewStat struct {
	Name            string
	Samples         int
	AvgOutputTokens float64
	AvgLatencyMs    float64
	ChangeFrequency float64
	AvgChangeAmount float64
	OverlapWithResp float64
	Staleness       int
}

// ---------- Renderers ----------

// writeStaticAssets copies the embedded style.css and viewer.js into the
// trace dir. Called once when the run starts so the directory is openable
// even mid-run.
func writeStaticAssets(dir string) error {
	for _, name := range staticAssetNames {
		data, err := viewerFS.ReadFile("viewer/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// renderIndex emits index.html for the current run. entries may be empty,
// in which case the page shows a "no iterations yet" placeholder.
func renderIndex(dir string, header RunHeader, entries []iterationLogEntry) error {
	iters := make([]indexIter, 0, len(entries))
	for _, e := range entries {
		iters = append(iters, newIndexIter(header, e))
	}
	var buf bytes.Buffer
	if err := traceTmpls.ExecuteTemplate(&buf, "index.html.tmpl", indexView{Header: header, Iterations: iters}); err != nil {
		return fmt.Errorf("render index: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, "index.html"), buf.Bytes())
}

// renderIteration emits one iter-NNNNN.html. prevN/nextN of zero means
// "no link"; the template renders a disabled placeholder. total is the
// current iteration count (used for "N of M" labels).
func renderIteration(dir string, header RunHeader, rec IterationTrace, prevN, nextN, total int) error {
	v := buildIterView(header, rec, prevN, nextN, total)
	var buf bytes.Buffer
	if err := traceTmpls.ExecuteTemplate(&buf, "iter.html.tmpl", v); err != nil {
		return fmt.Errorf("render iter %d: %w", rec.Iteration, err)
	}
	return writeFileAtomic(filepath.Join(dir, iterationHTMLName(rec.Iteration)), buf.Bytes())
}

// ---------- Builders ----------

func newIndexIter(h RunHeader, e iterationLogEntry) indexIter {
	rel := e.StartedAt.Sub(h.StartedAt)
	kind := e.TurnKind
	if kind == "" {
		kind = "work"
	}
	return indexIter{
		N:                   e.N,
		NPadded:             fmt.Sprintf("%0*d", tracePadding, e.N),
		WorkIteration:       e.WorkIteration,
		TurnKind:            kind,
		IsModeling:          kind == "modeling",
		HTMLFile:            iterationHTMLName(e.N),
		RelStart:            formatRelDuration(rel),
		DurationStr:         formatMs(e.DurationMs),
		InputTokens:         e.InputTokens,
		OutputTokens:        e.OutputTokens,
		StopReason:          e.StopReason,
		StopClass:           stopReasonClass(e.StopReason),
		Overflowed:          e.Overflowed,
		RevisionPromptFired: e.RevisionPromptFired,
		IdlePollsBefore:     e.IdlePollsBefore,
	}
}

func buildIterView(header RunHeader, rec IterationTrace, prevN, nextN, total int) iterView {
	durMs := rec.CompletedAt.Sub(rec.StartedAt).Milliseconds()
	budgetPct := 0.0
	if rec.Budget.LimitTokens > 0 {
		budgetPct = float64(rec.Budget.UsedTokens) / float64(rec.Budget.LimitTokens) * 100
		if budgetPct > 100 {
			budgetPct = 100
		}
	}
	kind := rec.TurnKind
	if kind == "" {
		kind = "work"
	}
	v := iterView{
		Header:          header,
		Iter:            rec,
		TurnKind:        kind,
		IsModeling:      kind == "modeling",
		DurationStr:     formatMs(durMs),
		IdleWaitStr:     formatMs(rec.IdleWaitMs),
		BudgetPercent:   budgetPct,
		TotalIterations: total,
		PrevN:           prevN,
		NextN:           nextN,
		Messages:        buildViewMessages(rec.Context.Messages, rec.Response, rec.ToolResults),
	}
	if prevN > 0 {
		v.PrevHref = iterationHTMLName(prevN)
	}
	if nextN > 0 {
		v.NextHref = iterationHTMLName(nextN)
	}
	if len(rec.StatsSnapshot) > 0 {
		v.HasStats = true
		names := make([]string, 0, len(rec.StatsSnapshot))
		for k := range rec.StatsSnapshot {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			s := rec.StatsSnapshot[name]
			if s == nil {
				continue
			}
			v.Stats = append(v.Stats, viewStat{
				Name:            name,
				Samples:         s.Samples,
				AvgOutputTokens: s.AvgOutputTokens,
				AvgLatencyMs:    s.AvgLatencyMs,
				ChangeFrequency: s.ChangeFrequency,
				AvgChangeAmount: s.AvgChangeAmount,
				OverlapWithResp: s.OverlapWithResp,
				Staleness:       s.Staleness,
			})
		}
	}
	return v
}

// buildViewMessages concatenates the input context, the assistant
// response, and the synthesized tool results into one chat-style slice.
func buildViewMessages(input []traceMessage, resp TraceResponse, toolResults []TraceToolResult) []viewMessage {
	out := make([]viewMessage, 0, len(input)+1+len(toolResults))
	for _, m := range input {
		out = append(out, convertMessage(m))
	}
	out = append(out, viewMessage{
		Role:      "assistant",
		RoleLabel: "assistant",
		Blocks:    blocksFromAssistant(resp.Content),
	})
	for _, tr := range toolResults {
		out = append(out, viewMessage{
			Role:       "tool_result",
			RoleLabel:  "tool result",
			ToolName:   tr.ToolName,
			ToolCallID: tr.ToolCallID,
			IsError:    tr.IsError,
			Anchor:     "tool-" + tr.ToolCallID,
			Blocks:     []viewBlock{{Kind: "tool_result_body", Text: tr.Content}},
		})
	}
	return out
}

func convertMessage(m traceMessage) viewMessage {
	v := viewMessage{Role: m.Role}
	switch m.Role {
	case "user":
		v.RoleLabel = "user"
		v.Blocks = blocksFromUser(m.Content)
	case "assistant":
		v.RoleLabel = "assistant"
		v.Blocks = blocksFromAssistant(m.Content)
	case "tool_result":
		v.RoleLabel = "tool result"
		v.ToolName = m.ToolName
		v.ToolCallID = m.ToolCallID
		v.IsError = m.IsError
		v.Anchor = "tool-" + m.ToolCallID
		var b strings.Builder
		for _, c := range m.Content {
			if t, ok := c["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
		v.Blocks = []viewBlock{{Kind: "tool_result_body", Text: b.String()}}
	}
	return v
}

func blocksFromUser(content []map[string]any) []viewBlock {
	out := make([]viewBlock, 0, len(content))
	for _, c := range content {
		text, _ := c["text"].(string)
		if name, tail, body, ok := parseProgramHeader(text); ok {
			out = append(out, viewBlock{
				Kind:               "program",
				Text:               body,
				ProgramName:        name,
				ProgramTail:        tail,
				ProgramExitNonzero: !strings.HasPrefix(tail, "exit=0"),
			})
			continue
		}
		if strings.HasPrefix(text, "[") {
			// Other bracketed annotations (e.g. "[YOUR GOAL]\n…").
			out = append(out, viewBlock{Kind: "note", Text: text})
			continue
		}
		out = append(out, viewBlock{Kind: "text", Text: text})
	}
	return out
}

func blocksFromAssistant(content []map[string]any) []viewBlock {
	out := make([]viewBlock, 0, len(content))
	for _, c := range content {
		kind, _ := c["kind"].(string)
		switch kind {
		case "text":
			text, _ := c["text"].(string)
			sig, _ := c["signature"].(string)
			out = append(out, viewBlock{Kind: "text", Text: text, Signature: sig})
		case "thinking":
			text, _ := c["text"].(string)
			sig, _ := c["signature"].(string)
			out = append(out, viewBlock{Kind: "thinking", Text: text, Signature: sig, SignaturePreview: previewSig(sig)})
		case "tool_call":
			id, _ := c["id"].(string)
			name, _ := c["name"].(string)
			out = append(out, viewBlock{
				Kind:       "tool_call",
				Text:       prettyArgs(c["arguments"]),
				ToolName:   name,
				ToolCallID: id,
				ToolAnchor: "tool-" + id,
			})
		}
	}
	return out
}

// parseProgramHeader recognizes "[program=NAME tail...]\n<body>" — the
// shape every program-output and dropped-sentinel block emits. Returns
// ok=false for any other text so the caller falls back to plain text or
// a generic "note" block.
func parseProgramHeader(s string) (name, tail, body string, ok bool) {
	if !strings.HasPrefix(s, "[program=") {
		return "", "", "", false
	}
	lineEnd := strings.IndexByte(s, '\n')
	head := s
	if lineEnd >= 0 {
		head = s[:lineEnd]
	}
	if !strings.HasSuffix(head, "]") {
		return "", "", "", false
	}
	inner := strings.TrimPrefix(head, "[")
	inner = strings.TrimSuffix(inner, "]")
	after := strings.TrimPrefix(inner, "program=")
	if sp := strings.IndexByte(after, ' '); sp >= 0 {
		name = after[:sp]
		tail = strings.TrimSpace(after[sp+1:])
	} else {
		name = after
	}
	if lineEnd >= 0 && lineEnd+1 <= len(s) {
		body = s[lineEnd+1:]
	}
	return name, tail, body, true
}

func prettyArgs(v any) string {
	var raw []byte
	switch x := v.(type) {
	case json.RawMessage:
		raw = x
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	case nil:
		return "{}"
	default:
		data, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		raw = data
	}
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func previewSig(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

// ---------- Small helpers ----------

func iterationHTMLName(n int) string {
	return fmt.Sprintf("iter-%0*d.html", tracePadding, n)
}

func formatMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

func formatRelDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return "0s"
	}
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

func stopReasonClass(s string) string {
	if s == "end" {
		return "stop-end"
	}
	return ""
}

func formatThousands(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.Itoa(n)
	var b strings.Builder
	L := len(s)
	for i, r := range s {
		if i > 0 && (L-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// writeFileAtomic writes data to path via tmp + rename so a process killed
// mid-write never leaves a partially-written HTML page.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
