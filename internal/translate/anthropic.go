package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// --- Anthropic Messages: encode a pivot request ------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    *anthropicChoice   `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   json.RawMessage  `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`
	Thinking  string           `json:"thinking,omitempty"`
	Signature string           `json:"signature,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// EncodeAnthropic renders a pivot request as an Anthropic Messages body.
func EncodeAnthropic(r *Request) ([]byte, error) {
	out := anthropicRequest{
		Model:         r.Model,
		MaxTokens:     r.MaxTokens,
		System:        r.System,
		Temperature:   r.Temperature,
		TopP:          r.TopP,
		StopSequences: r.Stop,
		Stream:        r.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}

	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			// Anthropic represents every tool result as a user message; several
			// parallel results that arrive consecutively become one message.
			n := len(out.Messages)
			if n == 0 || out.Messages[n-1].Role != "user" || !lastIsToolResult(out.Messages[n-1]) {
				out.Messages = append(out.Messages, anthropicMessage{Role: "user"})
				n = len(out.Messages)
			}
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   mustJSON(joinText(m.Parts)),
			})
		case "assistant":
			blocks := make([]anthropicBlock, 0, len(m.Parts)+len(m.ToolCalls))
			for _, p := range m.Parts {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
				}
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: toolInput(tc.Arguments),
				})
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
			}
		default: // user
			blocks := anthropicContent(m.Parts)
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: blocks})
			}
		}
	}

	switch {
	case len(r.Tools) == 0:
		// no tools, no tool_choice
	case r.ToolChoice != nil && r.ToolChoice.Mode == ToolChoiceNone:
		// Anthropic has no "none"; omitting the tools is the equivalent.
	case r.ToolChoice != nil && r.ToolChoice.Mode == ToolChoiceFunction:
		out.ToolChoice = &anthropicChoice{Type: "tool", Name: r.ToolChoice.Name}
	case r.ToolChoice != nil && r.ToolChoice.Mode == ToolChoiceRequired:
		out.ToolChoice = &anthropicChoice{Type: "any"}
	}
	if out.ToolChoice == nil && len(r.Tools) > 0 {
		out.ToolChoice = &anthropicChoice{Type: "auto"}
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: normalizeSchema(t.Schema),
		})
	}

	return json.Marshal(out)
}

func lastIsToolResult(m anthropicMessage) bool {
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return len(m.Content) > 0
}

func anthropicContent(parts []Part) []anthropicBlock {
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, anthropicBlock{Type: "text", Text: p.Text})
			}
		case "image":
			if p.Image == nil {
				continue
			}
			src := &anthropicSource{}
			switch {
			case p.Image.URL != "":
				src.Type, src.URL = "url", p.Image.URL
			case p.Image.Data != "":
				src.Type = "base64"
				src.MediaType = p.Image.MediaType
				src.Data = p.Image.Data
			default:
				continue
			}
			out = append(out, anthropicBlock{Type: "image", Source: src})
		}
	}
	return out
}

func toolInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		// A truncated tool call (common with streamed responses) must still be
		// sendable; wrap the raw text so the shape stays a JSON object.
		return mustJSON(map[string]string{"_raw": trimmed})
	}
	return json.RawMessage(trimmed)
}

// --- Anthropic Messages: decode a request ------------------------------------

type anthropicIncoming struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    json.RawMessage
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools         []anthropicTool  `json:"tools"`
	ToolChoice    *anthropicChoice `json:"tool_choice"`
	Temperature   *float64         `json:"temperature"`
	TopP          *float64         `json:"top_p"`
	StopSequences []string         `json:"stop_sequences"`
	Stream        bool             `json:"stream"`
}

// DecodeAnthropic parses an Anthropic Messages request into the pivot.
func DecodeAnthropic(body []byte) (*Request, error) {
	var wire anthropicIncoming
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode anthropic request: %w", err)
	}
	req := &Request{
		Model:       wire.Model,
		System:      decodeAnthropicSystem(wire.System),
		MaxTokens:   wire.MaxTokens,
		Temperature: wire.Temperature,
		TopP:        wire.TopP,
		Stop:        wire.StopSequences,
		Stream:      wire.Stream,
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = DefaultMaxTokens
	}

	for _, m := range wire.Messages {
		blocks, err := decodeAnthropicContent(m.Content)
		if err != nil {
			return nil, err
		}
		appendAnthropicMessage(req, m.Role, blocks)
	}

	for _, t := range wire.Tools {
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      normalizeSchema(t.InputSchema),
		})
	}
	if wire.ToolChoice != nil {
		switch wire.ToolChoice.Type {
		case "any":
			req.ToolChoice = &ToolChoice{Mode: ToolChoiceRequired}
		case "tool":
			req.ToolChoice = &ToolChoice{Mode: ToolChoiceFunction, Name: wire.ToolChoice.Name}
		default:
			req.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto}
		}
	}
	return req, nil
}

func decodeAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []Part
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, Part{Type: "text", Text: b.Text})
		}
	}
	return joinText(parts)
}

func decodeAnthropicContent(raw json.RawMessage) ([]anthropicBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []anthropicBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("anthropic message content: %w", err)
	}
	return blocks, nil
}

func appendAnthropicMessage(req *Request, role string, blocks []anthropicBlock) {
	var (
		parts     []Part
		toolCalls []ToolCall
		reasoning string
		results   []Message
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, Part{Type: "text", Text: b.Text})
			}
		case "image":
			if img := anthropicImage(b.Source); img != nil {
				parts = append(parts, Part{Type: "image", Image: img})
			}
		case "tool_use":
			toolCalls = append(toolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: compactJSON(b.Input),
			})
		case "tool_result":
			var text string
			var contentText string
			if json.Unmarshal(b.Content, &contentText) == nil {
				text = contentText
			} else {
				var inner []anthropicBlock
				if json.Unmarshal(b.Content, &inner) == nil {
					var innerParts []Part
					for _, ib := range inner {
						if ib.Type == "text" {
							innerParts = append(innerParts, Part{Type: "text", Text: ib.Text})
						}
					}
					text = joinText(innerParts)
				} else {
					text = string(b.Content)
				}
			}
			results = append(results, Message{
				Role:       "tool",
				Parts:      []Part{{Type: "text", Text: text}},
				ToolCallID: b.ToolUseID,
			})
		case "thinking":
			reasoning += b.Thinking
		}
	}

	// Tool results must precede the rest of the user turn in the pivot order,
	// because that is how the OpenAI wire represents them (separate messages).
	req.Messages = append(req.Messages, results...)
	if role == "assistant" {
		if len(parts) > 0 || len(toolCalls) > 0 || reasoning != "" {
			req.Messages = append(req.Messages, Message{
				Role: "assistant", Parts: parts, ToolCalls: toolCalls, Reasoning: reasoning,
			})
		}
		return
	}
	if len(parts) > 0 {
		req.Messages = append(req.Messages, Message{Role: "user", Parts: parts})
	}
}

func anthropicImage(src *anthropicSource) *Image {
	if src == nil {
		return nil
	}
	switch src.Type {
	case "base64":
		return &Image{MediaType: src.MediaType, Data: src.Data}
	case "url":
		return &Image{URL: src.URL}
	}
	return nil
}

// --- Anthropic Messages: decode a response -----------------------------------

type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// DecodeAnthropicResponse maps an Anthropic Messages response to the pivot.
func DecodeAnthropicResponse(body []byte) (*Response, error) {
	var wire anthropicResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	r := &Response{
		ID:           wire.ID,
		Model:        wire.Model,
		FinishReason: anthropicFinish(wire.StopReason),
		Usage: Usage{
			PromptTokens:     wire.Usage.InputTokens,
			CompletionTokens: wire.Usage.OutputTokens,
			CacheReadTokens:  wire.Usage.CacheReadInputTokens,
		},
	}
	var text strings.Builder
	for _, b := range wire.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			r.Reasoning += b.Thinking
		case "tool_use":
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: compactJSON(b.Input),
			})
		}
	}
	r.Content = text.String()
	return r, nil
}

func anthropicFinish(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// --- Anthropic Messages: encode a response -----------------------------------

type anthropicOutMessage struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

// EncodeAnthropicResponse renders a pivot response as an Anthropic message.
func EncodeAnthropicResponse(r *Response) ([]byte, error) {
	block := make([]anthropicBlock, 0, 2)
	if r.Content != "" {
		block = append(block, anthropicBlock{Type: "text", Text: r.Content})
	}
	for _, tc := range r.ToolCalls {
		block = append(block, anthropicBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: toolInput(tc.Arguments),
		})
	}
	if len(block) == 0 {
		block = []anthropicBlock{{Type: "text", Text: ""}}
	}
	out := anthropicOutMessage{
		ID:         r.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		Content:    block,
		StopReason: openAIFinishToAnthropic(r.FinishReason),
		Usage: anthropicUsage{
			InputTokens:          r.Usage.PromptTokens,
			OutputTokens:         r.Usage.CompletionTokens,
			CacheReadInputTokens: r.Usage.CacheReadTokens,
		},
	}
	return json.Marshal(out)
}

func openAIFinishToAnthropic(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// EncodeImageDataURL turns an inline image into the data: URL OpenAI clients
// use, so a pivot image can travel through the OpenAI wire unchanged. Data is
// already base64 from the provider, so it is copied rather than re-encoded.
func EncodeImageDataURL(img *Image) string {
	if img == nil {
		return ""
	}
	if img.URL != "" {
		return img.URL
	}
	return "data:" + img.MediaType + ";base64," + img.Data
}
