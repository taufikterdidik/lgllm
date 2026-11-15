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

func TestConnectionBreakdownIsSeparateFromModelWindow(t *testing.T) {
	r := New(2)
	base := time.Unix(2000, 0)
	// Two credentials serve the same model. The model window keeps the latest
	// two attempts overall; each credential keeps its own latest two.
	r.RecordConnection("p", "m", "a", Sample{At: base, OK: true, Stream: true, TPS: 10})
	r.RecordConnection("p", "m", "b", Sample{At: base.Add(time.Second), OK: false})
	r.RecordConnection("p", "m", "a", Sample{At: base.Add(2 * time.Second), OK: true, Stream: true, TPS: 30})
	r.RecordConnection("p", "m", "a", Sample{At: base.Add(3 * time.Second), OK: true, Stream: true, TPS: 40})

	model := r.Snapshot()
	if len(model) != 1 {
		t.Fatalf("model aggregates = %d, want 1", len(model))
	}
	if model[0].Attempts != 2 || model[0].Successes != 2 || model[0].Failures != 0 {
		t.Errorf("model window = %d/%d/%d, want the latest 2 attempts",
			model[0].Attempts, model[0].Successes, model[0].Failures)
	}
	if model[0].Connection != "" {
		t.Errorf("model aggregate connection = %q, want empty", model[0].Connection)
	}

	conns := r.SnapshotConnections()
	if len(conns) != 2 {
		t.Fatalf("connection aggregates = %d, want 2", len(conns))
	}
	byConn := make(map[string]Aggregate, len(conns))
	for _, a := range conns {
		byConn[a.Connection] = a
	}
	if a := byConn["a"]; a.Attempts != 2 || a.Successes != 2 || !a.Stream.TPSKnown {
		t.Errorf("connection a = %+v", a)
	}
	if a := byConn["b"]; a.Attempts != 1 || a.Failures != 1 {
		t.Errorf("connection b = %+v", a)
	}
	if got, want := byConn["a"].Stream.AvgTPS, float64(35); got != want {
		t.Errorf("connection a avg TPS = %v, want %v", got, want)
	}
}

func TestSeriesIsOldestFirstAndIncludesFailures(t *testing.T) {
	r := New(2)
	base := time.Unix(3000, 0)
	r.Record("p", "m", Sample{At: base, OK: true, Stream: true, TPS: 5})
	r.Record("p", "m", Sample{At: base.Add(time.Second), OK: false, Stream: true})
	r.Record("p", "m", Sample{At: base.Add(2 * time.Second), OK: true, Stream: true, TPS: 9})

	series := r.Series("p", "m")
	if len(series) != 2 {
		t.Fatalf("series = %d samples, want the window size 2", len(series))
	}
	if !series[0].At.Equal(base.Add(time.Second)) || series[0].OK || !series[1].OK {
		t.Errorf("series = %+v, want the latest two oldest-first", series)
	}
	if got := r.Series("p", "missing"); got != nil {
		t.Errorf("missing series = %+v, want nil", got)
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

func TestTapRecordsFinishAcrossFormats(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		payload string
		reason  string
	}{
		{
			name:   "openai",
			format: "openai",
			payload: `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n" +
				"data: [DONE]\n\n",
			reason: "stop",
		},
		{
			name:   "anthropic",
			format: "anthropic",
			payload: `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			reason: "end_turn",
		},
		{
			name:    "gemini",
			format:  "gemini",
			payload: `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}` + "\n\n",
			reason:  "STOP",
		},
		{
			name:   "responses",
			format: "openai-responses",
			payload: `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := readTap(t, tt.payload, tt.format, 5)
			if !stats.SawFinish() {
				t.Fatalf("finish was not recorded for %s", tt.name)
			}
			if tt.reason != "" && stats.FinishReason != tt.reason {
				t.Errorf("finish reason = %q, want %q", stats.FinishReason, tt.reason)
			}
		})
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
	if stats.SawFinish() {
		t.Error("a stream without finish_reason must not look finished")
	}
}
