// Package translate converts between the wire formats of the providers lgllm
// proxies. OpenAI Chat Completions is the pivot: a request from any dialect is
// decoded into one shape, and every target format is encoded from that shape.
// With N dialects that gives N decoders and N encoders instead of N² pairs.
package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is the pivot for a generation request. It is deliberately close to
// OpenAI Chat Completions, because that is what most clients speak and what the
// proxy's own ingress accepts.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool
	Tools       []Tool
	ToolChoice  *ToolChoice
	// ReasoningEffort is OpenAI's hint ("low"/"medium"/"high"). Providers that
	// do not share the scale ignore it rather than guessing a mapping.
	ReasoningEffort string
}

// Message is one turn. Content holds text and images; ToolCalls are assistant
// tool invocations; ToolCallID marks a tool result.
type Message struct {
	Role       string
	Parts      []Part
	ToolCalls  []ToolCall
	ToolCallID string
	// ToolError marks a failed tool result. The saver never compresses one: its
	// text is the error the model needs to act on.
	ToolError bool
	// Reasoning is assistant chain-of-thought. It is never sent to providers
	// that do not declare support for it.
	Reasoning string
}

// Part is a piece of message content.
type Part struct {
	Type  string // "text" | "image"
	Text  string
	Image *Image
}

// Image is either a remote URL or inline base64 data.
type Image struct {
	URL       string // http(s) URL, when set
	MediaType string // e.g. image/png, when inline
	Data      string // base64 payload, when inline
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is one function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON text, exactly as the model produced it
}

// ToolChoice modes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceFunction = "function"
)

// ToolChoice is how the caller wants tools used.
type ToolChoice struct {
	Mode string
	Name string // set when Mode == ToolChoiceFunction
}

// Response is the pivot for a non-streamed completion.
type Response struct {
	ID           string
	Model        string
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string // OpenAI vocabulary: stop|length|tool_calls|content_filter
	Usage        Usage
}

// Usage counts tokens. CacheRead is reported by providers that expose prompt
// caching and is zero otherwise.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CacheReadTokens  int
}

// DefaultMaxTokens is sent to providers that require max_tokens even when the
// client omits it, which is the case for every Anthropic request.
const DefaultMaxTokens = 4096

// --- OpenAI Chat Completions: decode request ---------------------------------

type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	Tools               []openAITool    `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ReasoningEffort     string          `json:"reasoning_effort"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"`
	Name             string           `json:"name"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
	ToolCallID       string           `json:"tool_call_id"`
	ReasoningContent string           `json:"reasoning_content"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio json.RawMessage `json:"input_audio"`
}

// DecodeOpenAI parses an OpenAI chat-completions request body.
func DecodeOpenAI(body []byte) (*Request, error) {
	var wire openAIRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode [OI] request: %w", err)
	}
	req := &Request{
		Model:           wire.Model,
		Temperature:     wire.Temperature,
		TopP:            wire.TopP,
		Stream:          wire.Stream,
		ReasoningEffort: wire.ReasoningEffort,
	}
	if wire.MaxTokens != nil {
		req.MaxTokens = *wire.MaxTokens
	} else if wire.MaxCompletionTokens != nil {
		req.MaxTokens = *wire.MaxCompletionTokens
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = DefaultMaxTokens
	}
	req.Stop = decodeStop(wire.Stop)

	for _, m := range wire.Messages {
		if err := appendOpenAIMessage(req, m); err != nil {
			return nil, err
		}
	}

	for _, t := range wire.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // built-in tools have no cross-provider equivalent
		}
		req.Tools = append(req.Tools, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      normalizeSchema(t.Function.Parameters),
		})
	}
	if len(req.Tools) > 0 {
		req.ToolChoice = decodeToolChoice(wire.ToolChoice)
	}
	return req, nil
}

func appendOpenAIMessage(req *Request, m openAIMessage) error {
	parts, err := decodeOpenAIContent(m.Content)
	if err != nil {
		return err
	}

	switch m.Role {
	case "system", "developer":
		text := joinText(parts)
		if text != "" {
			if req.System != "" {
				req.System += "\n\n"
			}
			req.System += text
		}
		return nil
	case "assistant":
		msg := Message{Role: "assistant", Parts: parts, Reasoning: m.ReasoningContent}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		req.Messages = append(req.Messages, msg)
		return nil
	case "tool", "function":
		req.Messages = append(req.Messages, Message{
			Role:       "tool",
			Parts:      parts,
			ToolCallID: m.ToolCallID,
		})
		return nil
	default: // user
		req.Messages = append(req.Messages, Message{Role: "user", Parts: parts})
		return nil
	}
}

func decodeOpenAIContent(raw json.RawMessage) ([]Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []Part{{Type: "text", Text: text}}, nil
	}
	var wire []openAIPart
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("message content is neither a string nor an array: %w", err)
	}
	parts := make([]Part, 0, len(wire))
	for _, p := range wire {
		switch p.Type {
		case "text", "":
			if p.Text != "" {
				parts = append(parts, Part{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			parts = append(parts, Part{Type: "image", Image: parseImageURL(p.ImageURL.URL)})
		default:
			// audio and file parts have no universal equivalent; dropping them
			// is better than failing the whole request.
		}
	}
	return parts, nil
}

func parseImageURL(url string) *Image {
	if strings.HasPrefix(url, "data:") {
		// data:image/png;base64,AAAA
		rest := strings.TrimPrefix(url, "data:")
		meta, data, ok := strings.Cut(rest, ",")
		if !ok {
			return &Image{URL: url}
		}
		mediaType := meta
		mediaType = strings.TrimSuffix(mediaType, ";base64")
		return &Image{MediaType: mediaType, Data: data}
	}
	return &Image{URL: url}
}

func decodeStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

func decodeToolChoice(raw json.RawMessage) *ToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			return &ToolChoice{Mode: mode}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return nil
	}
	return &ToolChoice{Mode: ToolChoiceFunction, Name: obj.Function.Name}
}

func joinText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func normalizeSchema(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

// --- OpenAI Chat Completions: encode response --------------------------------

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIRespMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIRespMsg struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// EncodeOpenAI renders a pivot response as OpenAI chat-completions JSON.
func EncodeOpenAI(r *Response) ([]byte, error) {
	out := openAIResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: now(),
		Model:   r.Model,
		Usage: openAIUsage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.PromptTokens + r.Usage.CompletionTokens,
		},
	}
	if r.Usage.CacheReadTokens > 0 {
		out.Usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: r.Usage.CacheReadTokens}
	}
	msg := openAIRespMsg{Role: "assistant", Content: r.Content, ReasoningContent: r.Reasoning}
	for _, tc := range r.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, toOpenAIToolCall(tc))
	}
	out.Choices = []openAIChoice{{
		Index:        0,
		Message:      msg,
		FinishReason: r.FinishReason,
	}}
	return json.Marshal(out)
}

// DecodeOpenAIResponse parses an OpenAI response body into the pivot. It is
// used by the Anthropic ingress, which has to translate back.
func DecodeOpenAIResponse(body []byte) (*Response, error) {
	var wire openAIResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode [OI] response: %w", err)
	}
	r := &Response{
		ID:    wire.ID,
		Model: wire.Model,
		Usage: Usage{
			PromptTokens:     wire.Usage.PromptTokens,
			CompletionTokens: wire.Usage.CompletionTokens,
		},
	}
	if wire.Usage.PromptTokensDetails != nil {
		r.Usage.CacheReadTokens = wire.Usage.PromptTokensDetails.CachedTokens
	}
	if len(wire.Choices) > 0 {
		c := wire.Choices[0]
		r.Content = c.Message.Content
		r.Reasoning = c.Message.ReasoningContent
		r.FinishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return r, nil
}

func toOpenAIToolCall(tc ToolCall) openAIToolCall {
	out := openAIToolCall{ID: tc.ID, Type: "function"}
	out.Function.Name = tc.Name
	out.Function.Arguments = tc.Arguments
	if out.Function.Arguments == "" {
		out.Function.Arguments = "{}"
	}
	return out
}

// DecodeAnthropicError reports the provider's own message so the caller does
// not have to translate an error it cannot improve.
var ErrEmptyBody = errors.New("empty body")

// ParseError extracts the human-readable part of a provider error document.
func ParseError(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message  string `json:"message"`
		ErrorMsg string `json:"error_msg"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		if envelope.Message != "" {
			return envelope.Message
		}
		if envelope.ErrorMsg != "" {
			return envelope.ErrorMsg
		}
	}
	return strings.TrimSpace(string(body))
}

// --- OpenAI Chat Completions: encode request ---------------------------------

type openAIOutRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIOutMsg  `json:"messages"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	StreamOptions   *streamOptions  `json:"stream_options,omitempty"`
	Tools           []openAIOutTool `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIOutMsg struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type openAIOutTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// EncodeOpenAIRequest renders the pivot as an OpenAI Chat Completions request.
// The Anthropic ingress uses it when the chosen provider speaks OpenAI.
func EncodeOpenAIRequest(r *Request) ([]byte, error) {
	out := openAIOutRequest{
		Model:           r.Model,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stop:            r.Stop,
		Stream:          r.Stream,
		ReasoningEffort: r.ReasoningEffort,
	}
	if r.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if r.System != "" {
		out.Messages = append(out.Messages, openAIOutMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		msg := openAIOutMsg{
			Role:             m.Role,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.Reasoning,
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, toOpenAIToolCall(tc))
			}
		}
		msg.Content = encodeOpenAIContent(m.Parts)
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range r.Tools {
		var tool openAIOutTool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = normalizeSchema(t.Schema)
		out.Tools = append(out.Tools, tool)
	}
	if len(r.Tools) > 0 && r.ToolChoice != nil {
		switch r.ToolChoice.Mode {
		case ToolChoiceNone:
			out.ToolChoice = json.RawMessage(`"none"`)
		case ToolChoiceRequired:
			out.ToolChoice = json.RawMessage(`"required"`)
		case ToolChoiceFunction:
			out.ToolChoice = mustJSON(map[string]any{
				"type": "function", "function": map[string]string{"name": r.ToolChoice.Name},
			})
		default:
			out.ToolChoice = json.RawMessage(`"auto"`)
		}
	}
	return json.Marshal(out)
}

func encodeOpenAIContent(parts []Part) any {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "image":
			url := EncodeImageDataURL(p.Image)
			if url == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "image_url", "image_url": map[string]string{"url": url},
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
