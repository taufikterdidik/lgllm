package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// --- Responses API: encode a pivot request -----------------------------------

type responsesRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions,omitempty"`
	Input        []responsesItem  `json:"input"`
	MaxOutput    int              `json:"max_output_tokens,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	TopP         *float64         `json:"top_p,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
	Tools        []responsesTool  `json:"tools,omitempty"`
	ToolChoice   json.RawMessage  `json:"tool_choice,omitempty"`
	Reasoning    *responsesReason `json:"reasoning,omitempty"`
	Store        *bool            `json:"store,omitempty"`
}

type responsesItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call / function_call_output
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type responsesContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesReason struct {
	Effort string `json:"effort,omitempty"`
}

// EncodeResponses renders a pivot request as an OpenAI Responses API body.
func EncodeResponses(r *Request) ([]byte, error) {
	no := false
	out := responsesRequest{
		Model:        r.Model,
		Instructions: r.System,
		MaxOutput:    r.MaxTokens,
		Temperature:  r.Temperature,
		TopP:         r.TopP,
		Stream:       r.Stream,
		Store:        &no,
	}
	if r.ReasoningEffort != "" {
		out.Reasoning = &responsesReason{Effort: r.ReasoningEffort}
	}

	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			out.Input = append(out.Input, responsesItem{
				Type: "function_call_output", CallID: m.ToolCallID, Output: joinText(m.Parts),
			})
		case "assistant":
			if text := joinText(m.Parts); text != "" {
				content, _ := json.Marshal([]responsesContent{{Type: "output_text", Text: text}})
				out.Input = append(out.Input, responsesItem{
					Type: "message", Role: "assistant", Content: content,
				})
			}
			for _, tc := range m.ToolCalls {
				out.Input = append(out.Input, responsesItem{
					Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
				})
			}
		default:
			parts := make([]responsesContent, 0, len(m.Parts))
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					if p.Text != "" {
						parts = append(parts, responsesContent{Type: "input_text", Text: p.Text})
					}
				case "image":
					if url := EncodeImageDataURL(p.Image); url != "" {
						parts = append(parts, responsesContent{Type: "input_image", ImageURL: url})
					}
				}
			}
			if len(parts) == 0 {
				continue
			}
			content, _ := json.Marshal(parts)
			out.Input = append(out.Input, responsesItem{
				Type: "message", Role: "user", Content: content,
			})
		}
	}

	for _, t := range r.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: normalizeSchema(t.Schema),
		})
	}
	if len(r.Tools) > 0 {
		out.ToolChoice = encodeResponsesToolChoice(r.ToolChoice)
	}
	return json.Marshal(out)
}

func encodeResponsesToolChoice(tc *ToolChoice) json.RawMessage {
	if tc == nil {
		return json.RawMessage(`"auto"`)
	}
	switch tc.Mode {
	case ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case ToolChoiceRequired:
		return json.RawMessage(`"required"`)
	case ToolChoiceFunction:
		return mustJSON(map[string]string{"type": "function", "name": tc.Name})
	default:
		return json.RawMessage(`"auto"`)
	}
}

// --- Responses API: decode a response ----------------------------------------

type responsesWire struct {
	ID     string            `json:"id"`
	Model  string            `json:"model"`
	Status string            `json:"status"`
	Output []responsesOutput `json:"output"`
	Usage  struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type responsesOutput struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

// DecodeResponsesResponse maps a Responses API body to the pivot.
func DecodeResponsesResponse(body []byte) (*Response, error) {
	var wire responsesWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode [OI] responses body: %w", err)
	}
	if wire.Error != nil && wire.Error.Message != "" {
		return nil, errors.New(wire.Error.Message)
	}
	r := &Response{
		ID:           wire.ID,
		Model:        wire.Model,
		FinishReason: "stop",
		Usage: Usage{
			PromptTokens:     wire.Usage.InputTokens,
			CompletionTokens: wire.Usage.OutputTokens,
			CacheReadTokens:  wire.Usage.InputTokensDetails.CachedTokens,
		},
	}
	var text strings.Builder
	for _, item := range wire.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				switch c.Type {
				case "output_text", "text":
					text.WriteString(c.Text)
				case "refusal":
					text.WriteString(c.Refusal)
					r.FinishReason = "content_filter"
				}
			}
		case "function_call":
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				ID: item.CallID, Name: item.Name, Arguments: item.Arguments,
			})
			r.FinishReason = "tool_calls"
		case "reasoning":
			for _, s := range item.Summary {
				r.Reasoning += s.Text
			}
		}
	}
	r.Content = text.String()
	if wire.Status == "incomplete" && wire.IncompleteDetails.Reason == "max_output_tokens" {
		r.FinishReason = "length"
	}
	return r, nil
}

// --- stream: Responses -> OpenAI ---------------------------------------------

type responsesEvent struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	ItemID string `json:"item_id"`
	Item   struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

func responsesToOpenAI(opts StreamOptions) Converter {
	return func(ctx context.Context, src io.Reader, dst io.Writer, flush func() error) error {
		r := newSSEReader(src)
		w := &sseWriter{w: dst, flush: flush}

		var (
			id        = "chatcmpl-stream"
			model     = opts.Model
			usage     = chunkUsage{}
			finish    = "stop"
			started   bool
			toolIndex = map[string]int{}
			nextTool  int
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
			if ev.Data == "" || ev.Data == "[DONE]" {
				if ev.Data == "[DONE]" {
					break
				}
				continue
			}
			var event responsesEvent
			if err := json.Unmarshal([]byte(ev.Data), &event); err != nil {
				continue
			}
			if event.Response.ID != "" && id == "chatcmpl-stream" {
				id = event.Response.ID
			}
			if event.Response.Model != "" && model == opts.Model {
				model = event.Response.Model
			}
			if !started {
				started = true
				if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
					return err
				}
			}

			switch event.Type {
			case "response.output_text.delta":
				if event.Delta != "" {
					if err := w.event(newChunk(id, model, chunkDelta{Content: event.Delta}, nil)); err != nil {
						return err
					}
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if event.Delta != "" {
					if err := w.event(newChunk(id, model, chunkDelta{ReasoningContent: event.Delta}, nil)); err != nil {
						return err
					}
				}
			case "response.output_item.added":
				if event.Item.Type == "function_call" {
					idx := nextTool
					nextTool++
					if event.Item.ID != "" {
						toolIndex[event.Item.ID] = idx
					}
					delta := chunkDelta{ToolCalls: []chunkTool{{
						Index: idx, ID: event.Item.CallID, Type: "function",
					}}}
					delta.ToolCalls[0].Function.Name = event.Item.Name
					if err := w.event(newChunk(id, model, delta, nil)); err != nil {
						return err
					}
				}
			case "response.function_call_arguments.delta":
				idx, ok := toolIndex[event.ItemID]
				if !ok {
					idx = nextTool
					nextTool++
					toolIndex[event.ItemID] = idx
				}
				if event.Delta != "" {
					delta := chunkDelta{ToolCalls: []chunkTool{{Index: idx}}}
					delta.ToolCalls[0].Function.Arguments = event.Delta
					if err := w.event(newChunk(id, model, delta, nil)); err != nil {
						return err
					}
				}
			case "response.completed", "response.incomplete":
				usage.PromptTokens = event.Response.Usage.InputTokens
				usage.CompletionTokens = event.Response.Usage.OutputTokens
				if event.Type == "response.incomplete" &&
					event.Response.IncompleteDetails.Reason == "max_output_tokens" {
					finish = "length"
				}
			}
		}

		if !started {
			if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
				return err
			}
		}
		if nextTool > 0 {
			finish = "tool_calls"
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		final := newChunk(id, model, chunkDelta{}, strPtr(finish))
		final.Usage = &usage
		if err := w.event(final); err != nil {
			return err
		}
		return w.raw("[DONE]")
	}
}
