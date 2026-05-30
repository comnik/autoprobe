package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/comnik/autoprobe/internal/provider"
)

const (
	// statsDirName is the directory under the probe root that holds one
	// JSON file per program. Per-program files avoid a serial merge step
	// in updateStats — each worker reads its own file, computes, and
	// writes — and let one corrupt stats file lose only its program's
	// history instead of the whole library's. The trade-off is that a
	// "full picture" view requires globbing N files; the revision script
	// does this and the agent can read individual files on demand.
	statsDirName = "statistics"

	// statsFileExt keeps the per-program files distinct from any other
	// content the agent might park in the statistics dir.
	statsFileExt = ".json"

	// EWMA smoothing factor. α≈0.1 gives an effective window of ~10
	// observations — recent enough to track phase changes in the task
	// without thrashing on individual-iteration noise.
	statsEWMAAlpha = 0.1
)

// programStats holds the cheap, always-on per-program metrics. The JSON
// tags define the on-disk layout at <root>/statistics/<name>.json; the
// revision script renders these into the prompt and the agent may also
// read individual files via the read tool.
type programStats struct {
	AvgOutputTokens float64 `json:"avg_output_tokens"`
	ChangeFrequency float64 `json:"change_frequency"`
	AvgChangeAmount float64 `json:"avg_change_amount"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	Staleness       int     `json:"staleness"`
	OverlapWithResp float64 `json:"overlap_with_response"`
	Samples         int     `json:"samples"`
}

func statsFilePath(root, programName string) string {
	return filepath.Join(root, statsDirName, programName+statsFileExt)
}

// loadStatsFor parses one program's stats file. Missing or unreadable
// returns nil so callers can construct a fresh record — that's the same
// path a brand-new program takes and avoids special-casing "first-ever
// observation" at the call site. A corrupt file also returns nil; the
// next save overwrites the bad bytes.
func loadStatsFor(root, programName string) *programStats {
	data, err := os.ReadFile(statsFilePath(root, programName))
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return nil
	}
	var s programStats
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return &s
}

// saveStatsFor writes one program's stats file, creating the statistics
// directory on first use. The file is JSON with a trailing newline so it
// behaves well when piped through `cat` or read directly into a terminal.
func saveStatsFor(root, programName string, s *programStats) error {
	dir := filepath.Join(root, statsDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(statsFilePath(root, programName), data, 0644)
}

// pruneStats removes stats files whose program is no longer installed.
// Keeps the statistics dir proportional to the current library — relevant
// because the revision script globs and renders every file. Files with a
// non-".json" extension are left alone so the agent can park sidecar
// notes (e.g. a README) without losing them.
func pruneStats(root string, live map[string]struct{}) error {
	dir := filepath.Join(root, statsDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), statsFileExt) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), statsFileExt)
		if _, alive := live[name]; alive {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// ewma blends a new observation into an EWMA. The first observation
// (n==0) seeds the average directly so it isn't biased toward zero;
// every subsequent observation is folded in at α weight.
func ewma(old, observation float64, n int, alpha float64) float64 {
	if n == 0 {
		return observation
	}
	return alpha*observation + (1-alpha)*old
}

// changeAmount returns 1 - 2·LCS(prev,curr)/(|prev|+|curr|), the line-level
// counterpart of Python's difflib.SequenceMatcher.ratio() inverted. The
// denominator is sum-of-lengths so the result is symmetric and bounded in
// [0, 1]: identical outputs score 0, fully disjoint outputs score 1.
// Inputs are split on '\n' with one trailing empty element stripped so an
// output that ends in '\n' isn't counted as having an extra blank line.
func changeAmount(prev, curr []byte) float64 {
	prevLines := splitLines(prev)
	currLines := splitLines(curr)
	total := len(prevLines) + len(currLines)
	if total == 0 {
		return 0
	}
	matched := lcsLineCount(prevLines, currLines)
	similarity := 2 * float64(matched) / float64(total)
	return 1 - similarity
}

// splitLines splits b on '\n' and drops a single trailing empty element
// produced by an output that ends with '\n'. Multiple trailing blanks are
// kept as-is — they're meaningful content, not formatting artifact.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// lcsLineCount returns the length of the longest common subsequence of
// two line slices using the standard O(n·m) DP with two rolling rows.
// The shorter input goes on the inner axis to keep row allocations small.
func lcsLineCount(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	n, m := len(a), len(b)
	prev := make([]int, n+1)
	curr := make([]int, n+1)
	for j := 1; j <= m; j++ {
		for i := 1; i <= n; i++ {
			if a[i-1] == b[j-1] {
				curr[i] = prev[i-1] + 1
			} else if curr[i-1] > prev[i] {
				curr[i] = curr[i-1]
			} else {
				curr[i] = prev[i]
			}
		}
		prev, curr = curr, prev
		for i := range curr {
			curr[i] = 0
		}
	}
	return prev[n]
}

// overlapWithResponse measures how much of a program's output appears,
// trigram-by-trigram, in the assistant's response. We compute recall (the
// fraction of program trigrams that appear in the response) rather than
// Jaccard because the question is "did the model use this program's
// output", not "are these two strings similar".
//
// Tokenization is lowercased word-level — any run of [a-z0-9] characters
// is a word, everything else is a separator — which is robust to the
// punctuation and bracket-heavy program-output headers without needing a
// real tokenizer. Outputs shorter than 3 words contribute zero.
func overlapWithResponse(programOutput, assistantText string) float64 {
	return overlapRecall(programOutput, wordTrigramSet(assistantText))
}

// overlapRecall is the hoisted variant: the assistant's trigram set is
// computed once by the caller and reused across every program in the
// iteration. Equivalent to overlapWithResponse for one-shot use.
func overlapRecall(programOutput string, responseTrigrams map[[3]string]struct{}) float64 {
	if len(responseTrigrams) == 0 {
		return 0
	}
	progTri := wordTrigramSet(programOutput)
	if len(progTri) == 0 {
		return 0
	}
	matched := 0
	for k := range progTri {
		if _, ok := responseTrigrams[k]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(progTri))
}

func wordTrigramSet(s string) map[[3]string]struct{} {
	words := tokenizeWords(s)
	if len(words) < 3 {
		return nil
	}
	out := make(map[[3]string]struct{}, len(words)-2)
	for i := 0; i+2 < len(words); i++ {
		out[[3]string{words[i], words[i+1], words[i+2]}] = struct{}{}
	}
	return out
}

func tokenizeWords(s string) []string {
	var words []string
	var cur []rune
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			cur = append(cur, r+('a'-'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cur = append(cur, r)
		default:
			if len(cur) > 0 {
				words = append(words, string(cur))
				cur = cur[:0]
			}
		}
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

// liveNamesFromResults extracts the set of program names that ran this
// iteration. Used by pruneStats to decide which stats files to remove.
func liveNamesFromResults(results []programResult) map[string]struct{} {
	out := make(map[string]struct{}, len(results))
	for _, r := range results {
		out[r.name] = struct{}{}
	}
	return out
}

// joinAssistantText concatenates the text and thinking content from an
// assistant message into a single string for overlap measurement. Tool
// calls are excluded — they're structured arguments, not narration, and
// including them would inflate overlap when the model echoes filenames.
func joinAssistantText(content []provider.AssistantContent) string {
	var b strings.Builder
	for _, c := range content {
		switch v := c.(type) {
		case provider.TextContent:
			if v.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(v.Text)
			}
		case provider.ThinkingContent:
			if v.Thinking != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(v.Thinking)
			}
		}
	}
	return b.String()
}
