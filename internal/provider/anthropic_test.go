package provider

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// ttlOf returns the cache_control TTL set on a content block, or "" if no
// breakpoint was placed on it.
func ttlOf(b anthropic.ContentBlockParamUnion) anthropic.CacheControlEphemeralTTL {
	switch {
	case b.OfText != nil:
		return b.OfText.CacheControl.TTL
	case b.OfToolResult != nil:
		return b.OfToolResult.CacheControl.TTL
	case b.OfToolUse != nil:
		return b.OfToolUse.CacheControl.TTL
	}
	return ""
}

// TestBuildMessagesNoCache: with caching off, no block carries a breakpoint.
func TestBuildMessagesNoCache(t *testing.T) {
	msgs := []Message{
		UserMessage{Content: []TextContent{
			{Text: "program output", CacheBreakpoint: true},
			{Text: "goal"},
		}},
	}
	out, err := buildAnthropicMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range out[0].Content {
		if ttlOf(b) != "" {
			t.Errorf("breakpoint placed despite cache=false")
		}
	}
}

// TestBuildMessagesBreakpoint3 places breakpoint 3 on the flagged block and
// the rolling breakpoint 4 on the final block — and nowhere else.
func TestBuildMessagesBreakpoint3(t *testing.T) {
	msgs := []Message{
		UserMessage{Content: []TextContent{
			{Text: "prog a"},
			{Text: "prog b", CacheBreakpoint: true}, // end of byte-stable region
			{Text: "exploration tail"},
			{Text: "goal"}, // last block → rolling breakpoint
		}},
	}
	out, err := buildAnthropicMessages(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	blocks := out[0].Content
	want := []anthropic.CacheControlEphemeralTTL{
		"", // prog a
		anthropic.CacheControlEphemeralTTLTTL5m, // prog b: breakpoint 3
		"", // exploration tail
		anthropic.CacheControlEphemeralTTLTTL5m, // goal: rolling breakpoint 4
	}
	for i, w := range want {
		if got := ttlOf(blocks[i]); got != w {
			t.Errorf("block %d TTL = %q, want %q", i, got, w)
		}
	}
}

// TestBuildMessagesRollingOnToolResult: when the conversation ends with tool
// results, the rolling breakpoint lands on the last tool_result block.
func TestBuildMessagesRollingOnToolResult(t *testing.T) {
	msgs := []Message{
		UserMessage{Content: []TextContent{{Text: "prog", CacheBreakpoint: true}}},
		AssistantMessage{Content: []AssistantContent{ToolCall{ID: "t1", Name: "x"}}},
		ToolResultMessage{ToolCallID: "t1", Content: []TextContent{{Text: "result"}}},
	}
	out, err := buildAnthropicMessages(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	// breakpoint 3 on the user message's program block
	if got := ttlOf(out[0].Content[0]); got != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Errorf("breakpoint 3 TTL = %q, want 5m", got)
	}
	// rolling breakpoint on the last (tool_result) message's last block
	last := out[len(out)-1].Content
	if got := ttlOf(last[len(last)-1]); got != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Errorf("rolling breakpoint TTL = %q, want 5m", got)
	}
}

// TestEphemeralCacheTTL maps the neutral CacheTTL to the SDK enum.
func TestEphemeralCacheTTL(t *testing.T) {
	if got := ephemeralCache(CacheTTL5m).TTL; got != anthropic.CacheControlEphemeralTTLTTL5m {
		t.Errorf("CacheTTL5m → %q, want 5m", got)
	}
	if got := ephemeralCache(CacheTTL1h).TTL; got != anthropic.CacheControlEphemeralTTLTTL1h {
		t.Errorf("CacheTTL1h → %q, want 1h", got)
	}
}
