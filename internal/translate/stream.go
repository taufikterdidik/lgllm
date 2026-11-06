package translate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Converter rewrites a provider's SSE stream into another dialect.
//
// flush must be called after each event so a client sees tokens as they arrive;
// the caller wires it to the HTTP response controller.
type Converter func(ctx context.Context, src io.Reader, dst io.Writer, flush func() error) error

// StreamOptions carries values a converter needs before the provider's first
// event has told it anything (the requested model, mostly).
type StreamOptions struct {
	Model string
}

// NewStreamConverter returns a converter that emits OpenAI-format SSE chunks.
// Providers that already speak OpenAI need no converter, and report false.
func NewStreamConverter(format string, opts StreamOptions) (Converter, bool) {
	switch format {
	case "anthropic":
		return anthropicToOpenAI(opts), true
	case "gemini":
		return geminiToOpenAI(opts), true
	case "openai-responses":
		return responsesToOpenAI(opts), true
	}
	return nil, false
}

// AnthropicIngressConverter returns a converter that rewrites an OpenAI-format
// SSE stream into Anthropic Messages events, for /v1/messages clients.
func AnthropicIngressConverter(model string) Converter {
	return openAIToAnthropic(model)
}

// sseEvent is one server-sent event.
type sseEvent struct {
	Event string
	Data  string
}

// sseReader parses the event-stream framing shared by every provider. A blank
// line terminates an event; comment lines and unknown fields are ignored.
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64<<10)}
}

func (s *sseReader) next() (sseEvent, error) {
	var (
		ev      sseEvent
		data    []string
		started bool
	)
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && (started || len(data) > 0) {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return sseEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !started && len(data) == 0 {
				continue // stray blank line between events
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
			started = true
		case "data":
			data = append(data, value)
			started = true
		}
	}
}

// sseWriter emits OpenAI-style "data: ..." lines.
type sseWriter struct {
	w     io.Writer
	flush func() error
}

func (s *sseWriter) event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if s.flush != nil {
		return s.flush()
	}
	return nil
}

func (s *sseWriter) raw(payload string) error {
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	if s.flush != nil {
		return s.flush()
	}
	return nil
}

// anthropicSSEWriter emits named Anthropic events.
type anthropicSSEWriter struct {
	w     io.Writer
	flush func() error
}

func (s *anthropicSSEWriter) event(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if s.flush != nil {
		return s.flush()
	}
	return nil
}

// chunk is the OpenAI streaming envelope.
type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chunkUsage   `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role             string      `json:"role,omitempty"`
	Content          string      `json:"content,omitempty"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	ToolCalls        []chunkTool `json:"tool_calls,omitempty"`
}

type chunkTool struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func newChunk(id, model string, delta chunkDelta, finish *string) chunk {
	return chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: now(),
		Model:   model,
		Choices: []chunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

func strPtr(s string) *string { return &s }

// --- stream: Anthropic -> OpenAI ---------------------------------------------

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func anthropicToOpenAI(opts StreamOptions) Converter {
	return func(ctx context.Context, src io.Reader, dst io.Writer, flush func() error) error {
		r := newSSEReader(src)
		w := &sseWriter{w: dst, flush: flush}

		var (
			id          = "chatcmpl-stream"
			model       = opts.Model
			usage       = chunkUsage{}
			finish      = "stop"
			haveFinish  bool
			toolIndex   int
			blockIsTool = map[int]bool{}
			toolIndexAt = map[int]int{}
			started     bool
		)

		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			ev, err := r.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			if ev.Data == "" {
				continue
			}
			if ev.Data == "[DONE]" {
				break
			}
			var evt anthropicStreamEvent
			if err := json.Unmarshal([]byte(ev.Data), &evt); err != nil {
				continue // ignore an event whose shape this proxy does not know
			}

			switch evt.Type {
			case "message_start":
				if evt.Message.ID != "" {
					id = evt.Message.ID
				}
				if evt.Message.Model != "" {
					model = evt.Message.Model
				}
				usage.PromptTokens = evt.Message.Usage.InputTokens
				usage.CompletionTokens = evt.Message.Usage.OutputTokens
				started = true
				if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
					return err
				}
			case "content_block_start":
				if !started {
					started = true
					if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
						return err
					}
				}
				if evt.ContentBlock.Type == "tool_use" {
					blockIsTool[evt.Index] = true
					idx := toolIndex
					toolIndex++
					toolIndexAt[evt.Index] = idx
					delta := chunkDelta{ToolCalls: []chunkTool{{
						Index: idx, ID: evt.ContentBlock.ID, Type: "function",
					}}}
					delta.ToolCalls[0].Function.Name = evt.ContentBlock.Name
					if err := w.event(newChunk(id, model, delta, nil)); err != nil {
						return err
					}
				}
			case "content_block_delta":
				switch evt.Delta.Type {
				case "text_delta":
					if evt.Delta.Text != "" {
						if err := w.event(newChunk(id, model, chunkDelta{Content: evt.Delta.Text}, nil)); err != nil {
							return err
						}
					}
				case "input_json_delta":
					if evt.Delta.PartialJSON != "" {
						idx := toolIndexAt[evt.Index]
						delta := chunkDelta{ToolCalls: []chunkTool{{Index: idx}}}
						delta.ToolCalls[0].Function.Arguments = evt.Delta.PartialJSON
						if err := w.event(newChunk(id, model, delta, nil)); err != nil {
							return err
						}
					}
				case "thinking_delta":
					if evt.Delta.Thinking != "" {
						if err := w.event(newChunk(id, model, chunkDelta{ReasoningContent: evt.Delta.Thinking}, nil)); err != nil {
							return err
						}
					}
				}
			case "message_delta":
				if evt.Delta.StopReason != "" {
					finish = anthropicFinish(evt.Delta.StopReason)
					haveFinish = true
				}
				if evt.Usage.OutputTokens > 0 {
					usage.CompletionTokens = evt.Usage.OutputTokens
				}
			case "message_stop":
				haveFinish = true
			}
		}

		if !started {
			// No events at all: emit a well-formed empty completion rather than
			// a silent connection, which clients report as a hung request.
			if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
				return err
			}
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if haveFinish {
			final := newChunk(id, model, chunkDelta{}, strPtr(finish))
			final.Usage = &usage
			if err := w.event(final); err != nil {
				return err
			}
		}
		return w.raw("[DONE]")
	}
}

// --- stream: OpenAI -> Anthropic (ingress) -----------------------------------

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chunkUsage `json:"usage"`
}

func openAIToAnthropic(model string) Converter {
	return func(ctx context.Context, src io.Reader, dst io.Writer, flush func() error) error {
		r := newSSEReader(src)
		w := &anthropicSSEWriter{w: dst, flush: flush}

		var (
			messageID  string
			textIndex  = -1
			toolBlocks = map[int]int{} // OpenAI tool index -> Anthropic block index
			nextBlock  int
			usage      anthropicUsage
			finish     = "end_turn"
			started    bool
			finished   bool
		)

		start := func() error {
			if started {
				return nil
			}
			started = true
			return w.event("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": messageID, "type": "message", "role": "assistant",
					"model": model, "content": []any{},
					"stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
				},
			})
		}

		closeBlocks := func() error {
			if textIndex >= 0 {
				if err := w.event("content_block_stop", map[string]any{
					"type": "content_block_stop", "index": textIndex,
				}); err != nil {
					return err
				}
				textIndex = -1
			}
			for _, bi := range toolBlocks {
				if err := w.event("content_block_stop", map[string]any{
					"type": "content_block_stop", "index": bi,
				}); err != nil {
					return err
				}
			}
			toolBlocks = map[int]int{}
			return nil
		}

		finishStream := func() error {
			if finished {
				return nil
			}
			finished = true
			if err := start(); err != nil {
				return err
			}
			if err := closeBlocks(); err != nil {
				return err
			}
			if err := w.event("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": finish, "stop_sequence": nil},
				"usage": map[string]int{"output_tokens": usage.OutputTokens},
			}); err != nil {
				return err
			}
			if err := w.event("message_stop", map[string]any{"type": "message_stop"}); err != nil {
				return err
			}
			return nil
		}

		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			ev, err := r.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			if ev.Data == "" {
				continue
			}
			if ev.Data == "[DONE]" {
				break
			}
			var c openAIChunk
			if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
				continue
			}
			if c.ID != "" {
				messageID = c.ID
			}
			if c.Model != "" {
				model = c.Model
			}
			if c.Usage != nil {
				usage.InputTokens = c.Usage.PromptTokens
				usage.OutputTokens = c.Usage.CompletionTokens
			}
			if err := start(); err != nil {
				return err
			}
			for _, ch := range c.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					bi, ok := toolBlocks[tc.Index]
					if !ok {
						bi = nextBlock
						toolBlocks[tc.Index] = bi
						nextBlock++
						if err := w.event("content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": bi,
							"content_block": map[string]any{
								"type": "tool_use", "id": tc.ID, "name": tc.Function.Name,
								"input": map[string]any{},
							},
						}); err != nil {
							return err
						}
					}
					if tc.Function.Arguments != "" {
						if err := w.event("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": bi,
							"delta": map[string]string{
								"type": "input_json_delta", "partial_json": tc.Function.Arguments,
							},
						}); err != nil {
							return err
						}
					}
				}
				if ch.Delta.Content != "" || (ch.Delta.ReasoningContent != "" && textIndex < 0) {
					if textIndex < 0 {
						textIndex = nextBlock
						nextBlock++
						if err := w.event("content_block_start", map[string]any{
							"type":  "content_block_start",
							"index": textIndex,
							"content_block": map[string]any{
								"type": "text", "text": "",
							},
						}); err != nil {
							return err
						}
					}
					if ch.Delta.Content != "" {
						if err := w.event("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": textIndex,
							"delta": map[string]string{"type": "text_delta", "text": ch.Delta.Content},
						}); err != nil {
							return err
						}
					}
				}
				if ch.FinishReason != nil {
					finish = openAIFinishToAnthropic(*ch.FinishReason)
				}
			}
		}

		if !started {
			if err := start(); err != nil {
				return err
			}
		}
		if err := finishStream(); err != nil {
			return err
		}
		return nil
	}
}
