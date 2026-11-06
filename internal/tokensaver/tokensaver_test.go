package tokensaver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"lgllm/internal/translate"
)

func toolMessage(text string) translate.Message {
	return translate.Message{
		Role:  "tool",
		Parts: []translate.Part{{Type: "text", Text: text}},
	}
}

func bigLog(lines int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "2026-11-07T10:00:00Z INFO worker %d: processing task id=%d\n", i, i)
	}
	return b.String()
}

func TestCompressesOnlyToolResults(t *testing.T) {
	big := bigLog(200)
	req := &translate.Request{
		System: "be brief",
		Messages: []translate.Message{
			{Role: "user", Parts: []translate.Part{{Type: "text", Text: big}}},
			{Role: "assistant", Parts: []translate.Part{{Type: "text", Text: big}}},
			toolMessage(big),
		},
	}

	stats := Apply(req, Options{MinBytes: 512, MaxBytes: 1024})
	if !stats.Applied || stats.Saved <= 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(req.Messages[0].Parts[0].Text) != len(big) {
		t.Error("a user message was modified")
	}
	if len(req.Messages[1].Parts[0].Text) != len(big) {
		t.Error("an assistant message was modified")
	}
	got := req.Messages[2].Parts[0].Text
	if !strings.Contains(got, Marker) {
		t.Error("compressed content has no marker")
	}
	if !strings.Contains(got, "10:00:00Z INFO worker 0") {
		t.Error("the head was not preserved")
	}
	if !strings.Contains(got, "worker 199") {
		t.Error("the tail was not preserved")
	}
	if len(got) >= len(big) {
		t.Errorf("content was not shrunk: %d -> %d", len(big), len(got))
	}
}

func TestSkipsSmallErrorAndIdempotent(t *testing.T) {
	stats := Apply(&translate.Request{Messages: []translate.Message{toolMessage("small")}},
		Options{MinBytes: 512})
	if stats.Applied {
		t.Error("a small result must not be compressed")
	}

	errMsg := toolMessage(bigLog(200))
	errMsg.ToolError = true
	stats = Apply(&translate.Request{Messages: []translate.Message{errMsg}}, Options{MinBytes: 512})
	if stats.Applied {
		t.Error("a failed tool result must not be compressed")
	}

	first := &translate.Request{Messages: []translate.Message{toolMessage(bigLog(200))}}
	Apply(first, Options{MinBytes: 512, MaxBytes: 1024})
	before := first.Messages[0].Parts[0].Text

	second := &translate.Request{Messages: []translate.Message{toolMessage(before)}}
	stats = Apply(second, Options{MinBytes: 512, MaxBytes: 1024})
	if stats.Applied {
		t.Error("content with the marker must be left alone")
	}
	if second.Messages[0].Parts[0].Text != before {
		t.Error("an idempotent pass changed the content")
	}
}

func TestJSONIsMinifiedAndDropsVerboseFields(t *testing.T) {
	type item struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		NodeID    string `json:"node_id"`
		AvatarURL string `json:"avatar_url"`
		CreatedAt string `json:"created_at"`
	}
	items := make([]item, 0, 80)
	for i := 0; i < 80; i++ {
		items = append(items, item{
			ID: i, Name: strings.Repeat("x", 40),
			NodeID:    strings.Repeat("n", 40),
			AvatarURL: strings.Repeat("a", 60),
			CreatedAt: "2026-01-01T00:00:00Z",
		})
	}
	raw, _ := json.MarshalIndent(items, "", "  ")

	req := &translate.Request{Messages: []translate.Message{toolMessage(string(raw))}}
	stats := Apply(req, Options{MinBytes: 512, MaxBytes: 4096})
	if !stats.Applied {
		t.Fatal("JSON was not compressed")
	}
	out := req.Messages[0].Parts[0].Text
	for _, dropped := range []string{"node_id", "avatar_url", "created_at"} {
		if strings.Contains(out, dropped) {
			t.Errorf("verbose field %q survived", dropped)
		}
	}
	if !strings.Contains(out, `"id"`) || !strings.Contains(out, `"name"`) {
		t.Error("useful fields were dropped")
	}
}

func TestDiffKeepsChangesAndDropsContext(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("index 123..456 100644\n")
	b.WriteString("--- a/main.go\n+++ b/main.go\n")
	b.WriteString("@@ -1,40 +1,40 @@\n")
	for i := 0; i < 30; i++ {
		b.WriteString(" context line that is not interesting\n")
	}
	b.WriteString("-old line\n+new line\n")
	for i := 0; i < 30; i++ {
		b.WriteString(" more context\n")
	}

	req := &translate.Request{Messages: []translate.Message{toolMessage(b.String())}}
	stats := Apply(req, Options{MinBytes: 128})
	if !stats.Applied {
		t.Fatal("diff was not compressed")
	}
	out := req.Messages[0].Parts[0].Text
	if !strings.Contains(out, "-old line") || !strings.Contains(out, "+new line") {
		t.Error("the change was dropped")
	}
	if !strings.Contains(out, "diff --git") || !strings.Contains(out, "@@") {
		t.Error("file and hunk headers were dropped")
	}
	if strings.Count(out, "context line") > 4 {
		t.Errorf("context was not trimmed: %d lines", strings.Count(out, "context line"))
	}
	if strings.Contains(out, "index 123..456") {
		t.Error("diff metadata survived")
	}
}

func TestRepeatedLogLinesCollapse(t *testing.T) {
	text := strings.Repeat("ERROR connection reset by peer\n", 50) + "final line\n"
	req := &translate.Request{Messages: []translate.Message{toolMessage(text)}}
	stats := Apply(req, Options{MinBytes: 64})
	if !stats.Applied {
		t.Fatal("log was not compressed")
	}
	out := req.Messages[0].Parts[0].Text
	if !strings.Contains(out, "repeated 50 times") {
		t.Errorf("no repetition marker:\n%s", out)
	}
	if !strings.Contains(out, "final line") {
		t.Error("the tail was dropped")
	}
}

func TestPromptIsAddedOnce(t *testing.T) {
	req := &translate.Request{System: "base", Messages: []translate.Message{toolMessage("short")}}
	Apply(req, Options{Prompt: "answer tersely"})
	if req.System != "base\n\nanswer tersely" {
		t.Errorf("system = %q", req.System)
	}
	Apply(req, Options{Prompt: "answer tersely"})
	if strings.Count(req.System, "answer tersely") != 1 {
		t.Errorf("prompt was added twice: %q", req.System)
	}
}
