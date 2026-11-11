package perf

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestWindowKeepsLatestSamples(t *testing.T) {
	r := New(3)
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		r.Record("p", "m", Sample{
			At:          base.Add(time.Duration(i) * time.Second),
			OK:          i%2 == 0,
			Stream:      true,
			TTFT:        time.Duration(10+i) * time.Millisecond,
			Latency:     time.Duration(100+i) * time.Millisecond,
			Tokens:      10 + i,
			TokensKnown: true,
			TPS:         float64(10 + i),
		})
	}

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("aggregates = %d, want 1", len(snap))
	}
	a := snap[0]
	if a.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", a.Attempts)
	}
	if a.Successes != 2 || a.Failures != 1 {
		t.Errorf("successes/failures = %d/%d, want 2/1", a.Successes, a.Failures)
	}
	if a.Stream.Samples != 2 {
		t.Errorf("stream samples = %d, want 2", a.Stream.Samples)
	}
	if got, want := a.Stream.AvgTTFT, 13*time.Millisecond; got != want {
		t.Errorf("avg TTFT = %v, want %v", got, want)
	}
	if got, want := a.Stream.AvgLatency, 103*time.Millisecond; got != want {
		t.Errorf("avg latency = %v, want %v", got, want)
	}
	if !a.Stream.TPSKnown || a.Stream.AvgTPS != 13 {
		t.Errorf("avg TPS = %v (known %v), want 13", a.Stream.AvgTPS, a.Stream.TPSKnown)
	}
	if want := base.Add(4 * time.Second); !a.LastSeen.Equal(want) {
		t.Errorf("last seen = %v, want %v", a.LastSeen, want)
	}
	if a.NonStream.Samples != 0 {
		t.Errorf("non-stream samples = %d, want 0", a.NonStream.Samples)
	}
}

func TestModesAreSeparatedAndFailuresIgnored(t *testing.T) {
	r := New(DefaultWindow)
	r.Record("p", "m", Sample{OK: true, Stream: true, TTFT: 100 * time.Millisecond,
		Latency: time.Second, Tokens: 50, TokensKnown: true, TPS: 25})
	r.Record("p", "m", Sample{OK: true, Stream: false, TTFT: 800 * time.Millisecond,
		Latency: 800 * time.Millisecond, Tokens: 40, TokensKnown: true, TPS: 50,
		TPSEstimated: true})
	r.Record("p", "m", Sample{OK: false, Stream: true, Latency: 3 * time.Second})
	r.Record("p", "m", Sample{OK: true, Stream: true, TTFT: 200 * time.Millisecond,
		Latency: 2 * time.Second, TokensKnown: false})

	a := r.Snapshot()[0]
	if a.Attempts != 4 || a.Successes != 3 || a.Failures != 1 {
		t.Fatalf("attempts/successes/failures = %d/%d/%d", a.Attempts, a.Successes, a.Failures)
	}
	if a.Stream.Samples != 2 || a.NonStream.Samples != 1 {
		t.Errorf("samples stream/non-stream = %d/%d", a.Stream.Samples, a.NonStream.Samples)
	}
	if got, want := a.Stream.AvgTTFT, 150*time.Millisecond; got != want {
		t.Errorf("stream avg TTFT = %v, want %v (failure excluded)", got, want)
	}
	if !a.Stream.TokensKnown {
		t.Error("stream tokens known = false, want true from the first sample")
	}
	if !a.NonStream.TPSEstimated || a.NonStream.AvgTPS != 50 {
		t.Errorf("non-stream TPS = %v (est %v)", a.NonStream.AvgTPS, a.NonStream.TPSEstimated)
	}
}

func TestSnapshotIsSorted(t *testing.T) {
	r := New(3)
	r.Record("z", "a", Sample{OK: true})
	r.Record("a", "z", Sample{OK: true})
	r.Record("a", "b", Sample{OK: true})

	snap := r.Snapshot()
	got := []string{snap[0].Provider + "/" + snap[0].Model, snap[1].Provider + "/" + snap[1].Model, snap[2].Provider + "/" + snap[2].Model}
	want := []string{"a/b", "a/z", "z/a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func readTap(t *testing.T, payload string, format string, chunk int) TapStats {
	t.Helper()
	tap := NewSSETap(io.NopCloser(strings.NewReader(payload)), format)
	defer tap.Close()

	var got strings.Builder
	buf := make([]byte, chunk)
	for {
		n, err := tap.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got.String() != payload {
		t.Errorf("tap changed the stream:\n got %q\nwant %q", got.String(), payload)
	}
	return tap.Stats()
}

func TestTapOpenAI(t *testing.T) {
	payload := ": keep-alive\n\n" +
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"

	stats := readTap(t, payload, "openai", 7) // small chunks split lines
	if stats.FirstContent.IsZero() {
		t.Error("first content was not recorded")
	}
	if stats.LastContent.Before(stats.FirstContent) {
		t.Errorf("last content %v is before first %v", stats.LastContent, stats.FirstContent)
	}
	if !stats.TokensKnown || stats.PromptTokens != 11 || stats.CompletionTokens != 7 {
		t.Errorf("usage = %+v", stats)
	}
}

func TestTapAnthropic(t *testing.T) {
	payload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	stats := readTap(t, payload, "anthropic", 11)
	if stats.FirstContent.IsZero() {
		t.Error("first content was not recorded")
	}
	if !stats.TokensKnown || stats.PromptTokens != 9 || stats.CompletionTokens != 5 {
		t.Errorf("usage = %+v", stats)
	}
}

func TestTapGemini(t *testing.T) {
	payload := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}` + "\n\n"
	stats := readTap(t, payload, "gemini", 13)
	if stats.FirstContent.IsZero() {
		t.Error("first content was not recorded")
	}
	if !stats.TokensKnown || stats.PromptTokens != 4 || stats.CompletionTokens != 2 {
		t.Errorf("usage = %+v", stats)
	}
}

func TestTapResponses(t *testing.T) {
	payload := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"Hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":6,"output_tokens":3}}}` + "\n\n"

	stats := readTap(t, payload, "openai-responses", 5)
	if stats.FirstContent.IsZero() {
		t.Error("first content was not recorded")
	}
	if !stats.TokensKnown || stats.PromptTokens != 6 || stats.CompletionTokens != 3 {
		t.Errorf("usage = %+v", stats)
	}
}

func TestTapWithoutUsage(t *testing.T) {
	payload := `data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	stats := readTap(t, payload, "openai", 64)
	if stats.FirstContent.IsZero() {
		t.Error("first content was not recorded")
	}
	if stats.TokensKnown {
		t.Error("tokens must stay unknown when the provider sends no usage")
	}
}
