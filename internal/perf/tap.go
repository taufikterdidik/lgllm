package perf

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"lgllm/internal/translate"
)

// TapStats is what an SSETap learned while a stream was relayed.
type TapStats struct {
	FirstContent     time.Time
	LastContent      time.Time
	PromptTokens     int
	CompletionTokens int
	// TokensKnown reports whether the provider sent a usage event. A provider
	// that never reports usage yields n/a rather than an invented count.
	TokensKnown bool
}

// ContentDuration is the time between the first and last content event. It is
// the denominator of a streamed TPS; zero when the stream had one content
// event or none at all.
func (s TapStats) ContentDuration() time.Duration {
	if s.FirstContent.IsZero() || !s.LastContent.After(s.FirstContent) {
		return 0
	}
	return s.LastContent.Sub(s.FirstContent)
}

// SSETap wraps a server-sent-event body and observes it without changing a
// byte. Relay code reads through the tap, so a passthrough stream stays
// byte-identical to the provider's response.
type SSETap struct {
	r      io.ReadCloser
	format string

	// mu guards the parser state, because a stall watchdog or a cancelled
	// context can Close the body while the relay goroutine is reading it.
	mu      sync.Mutex
	buf     []byte
	data    []string
	event   string
	stats   TapStats
	flushed bool
}

// NewSSETap wraps body. The format selects the usage and content shape to
// look for; unknown formats are treated as OpenAI Chat.
func NewSSETap(body io.ReadCloser, format string) *SSETap {
	return &SSETap{r: body, format: format}
}

// Read implements io.Reader, scanning complete SSE lines as they arrive.
func (t *SSETap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.scan(p[:n])
		t.mu.Unlock()
	}
	if err != nil {
		t.flush()
	}
	return n, err
}

// Close flushes a final event without a trailing blank line, then closes the
// upstream body.
func (t *SSETap) Close() error {
	t.flush()
	return t.r.Close()
}

// Stats returns the observations made so far.
func (t *SSETap) Stats() TapStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

func (t *SSETap) scan(chunk []byte) {
	t.buf = append(t.buf, chunk...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			return
		}
		line := string(t.buf[:i])
		t.buf = t.buf[i+1:]
		t.line(strings.TrimSuffix(line, "\r"), time.Now())
	}
}

func (t *SSETap) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.flushed {
		return
	}
	t.flushed = true
	if len(t.buf) > 0 {
		line := strings.TrimSuffix(string(t.buf), "\r")
		t.buf = nil
		if line != "" {
			t.line(line, time.Now())
		}
	}
	t.endEvent(time.Now())
}

func (t *SSETap) line(line string, at time.Time) {
	if line == "" {
		t.endEvent(at)
		return
	}
	if strings.HasPrefix(line, ":") {
		return // comment / keep-alive
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		t.event = value
	case "data":
		t.data = append(t.data, value)
	}
}

func (t *SSETap) endEvent(at time.Time) {
	if len(t.data) == 0 {
		t.event = ""
		return
	}
	payload := strings.Join(t.data, "\n")
	t.data = t.data[:0]
	t.event = ""
	if payload == "" || payload == "[DONE]" {
		return
	}
	t.observe(payload, at)
}

func (t *SSETap) observe(payload string, at time.Time) {
	switch t.format {
	case translate.FormatAnthropic:
		t.observeAnthropic(payload, at)
	case translate.FormatGemini:
		t.observeGemini(payload, at)
	case translate.FormatResponses:
		t.observeResponses(payload, at)
	default:
		t.observeOpenAI(payload, at)
	}
}

// present reports whether a raw JSON member carries a value (not absent and
// not an explicit null).
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

func (t *SSETap) markContent(at time.Time) {
	if t.stats.FirstContent.IsZero() {
		t.stats.FirstContent = at
	}
	if at.After(t.stats.LastContent) {
		t.stats.LastContent = at
	}
}

func (t *SSETap) observeOpenAI(payload string, at time.Time) {
	var ev struct {
		Choices []struct {
			Delta struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
				FunctionCall     json.RawMessage   `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Usage != nil {
		t.stats.PromptTokens = ev.Usage.PromptTokens
		t.stats.CompletionTokens = ev.Usage.CompletionTokens
		t.stats.TokensKnown = true
	}
	for _, ch := range ev.Choices {
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" ||
			len(ch.Delta.ToolCalls) > 0 || present(ch.Delta.FunctionCall) {
			t.markContent(at)
			return
		}
	}
}

func (t *SSETap) observeAnthropic(payload string, at time.Time) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Delta struct {
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Message != nil {
		t.stats.PromptTokens = ev.Message.Usage.InputTokens
		t.stats.CompletionTokens = ev.Message.Usage.OutputTokens
		t.stats.TokensKnown = true
	}
	if ev.Type == "message_delta" && ev.Usage.OutputTokens > 0 {
		t.stats.CompletionTokens = ev.Usage.OutputTokens
		t.stats.TokensKnown = true
	}
	content := (ev.Type == "content_block_delta" &&
		(ev.Delta.Text != "" || ev.Delta.PartialJSON != "" || ev.Delta.Thinking != "")) ||
		(ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use")
	if content {
		t.markContent(at)
	}
}

func (t *SSETap) observeGemini(payload string, at time.Time) {
	var ev struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string          `json:"text"`
					FunctionCall json.RawMessage `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.UsageMetadata != nil {
		t.stats.PromptTokens = ev.UsageMetadata.PromptTokenCount
		t.stats.CompletionTokens = ev.UsageMetadata.CandidatesTokenCount
		t.stats.TokensKnown = true
	}
	for _, c := range ev.Candidates {
		for _, p := range c.Content.Parts {
			if p.Text != "" || present(p.FunctionCall) {
				t.markContent(at)
				return
			}
		}
	}
}

func (t *SSETap) observeResponses(payload string, at time.Time) {
	var ev struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Response != nil && ev.Response.Usage != nil {
		t.stats.PromptTokens = ev.Response.Usage.InputTokens
		t.stats.CompletionTokens = ev.Response.Usage.OutputTokens
		t.stats.TokensKnown = true
	}
	// Output text, reasoning and function-call argument deltas all carry
	// generated tokens; lifecycle events such as response.created do not.
	if strings.HasSuffix(ev.Type, ".delta") && ev.Delta != "" &&
		(strings.Contains(ev.Type, "output_text") ||
			strings.Contains(ev.Type, "function_call_arguments") ||
			strings.Contains(ev.Type, "reasoning")) {
		t.markContent(at)
	}
}
