package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// --- Gemini generateContent: encode a pivot request --------------------------

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTools    `json:"tools,omitempty"`
	ToolConfig        *geminiToolConf  `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiBlob             `json:"inlineData,omitempty"`
	FileData         *geminiFile             `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFile struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiGenConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiTools struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiToolConf struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

// EncodeGemini renders a pivot request as a native generateContent body.
func EncodeGemini(r *Request) ([]byte, error) {
	out := geminiRequest{
		GenerationConfig: &geminiGenConfig{
			MaxOutputTokens: r.MaxTokens,
			Temperature:     r.Temperature,
			TopP:            r.TopP,
			StopSequences:   r.Stop,
		},
	}
	if r.System != "" {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: r.System}}}
	}

	// Gemini names tool results after the function, but the pivot only carries
	// the call id. Remember the names from the assistant turns.
	callNames := map[string]string{}
	for _, m := range r.Messages {
		for _, tc := range m.ToolCalls {
			callNames[tc.ID] = tc.Name
		}
	}

	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			name := callNames[m.ToolCallID]
			if name == "" {
				name = "tool"
			}
			out.Contents = append(out.Contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{
					FunctionResponse: &geminiFunctionResponse{
						Name:     name,
						Response: toolResponse(joinText(m.Parts)),
					},
				}},
			})
		case "assistant":
			parts := geminiParts(m.Parts)
			for _, tc := range m.ToolCalls {
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					Name: tc.Name,
					Args: toolInput(tc.Arguments),
				}})
			}
			if len(parts) > 0 {
				out.Contents = append(out.Contents, geminiContent{Role: "model", Parts: parts})
			}
		default:
			parts := geminiParts(m.Parts)
			if len(parts) > 0 {
				out.Contents = append(out.Contents, geminiContent{Role: "user", Parts: parts})
			}
		}
	}

	if len(r.Tools) > 0 {
		decls := make([]geminiFunctionDecl, 0, len(r.Tools))
		for _, t := range r.Tools {
			decls = append(decls, geminiFunctionDecl{
				Name: t.Name, Description: t.Description, Parameters: normalizeSchema(t.Schema),
			})
		}
		out.Tools = []geminiTools{{FunctionDeclarations: decls}}
		out.ToolConfig = &geminiToolConf{}
		switch {
		case r.ToolChoice == nil || r.ToolChoice.Mode == ToolChoiceAuto:
			out.ToolConfig.FunctionCallingConfig.Mode = "AUTO"
		case r.ToolChoice.Mode == ToolChoiceNone:
			out.ToolConfig.FunctionCallingConfig.Mode = "NONE"
		case r.ToolChoice.Mode == ToolChoiceRequired:
			out.ToolConfig.FunctionCallingConfig.Mode = "ANY"
		case r.ToolChoice.Mode == ToolChoiceFunction:
			out.ToolConfig.FunctionCallingConfig.Mode = "ANY"
			out.ToolConfig.FunctionCallingConfig.AllowedFunctionNames = []string{r.ToolChoice.Name}
		}
	}
	return json.Marshal(out)
}

func geminiParts(parts []Part) []geminiPart {
	out := make([]geminiPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, geminiPart{Text: p.Text})
			}
		case "image":
			if p.Image == nil {
				continue
			}
			switch {
			case p.Image.URL != "":
				out = append(out, geminiPart{FileData: &geminiFile{FileURI: p.Image.URL}})
			case p.Image.Data != "":
				out = append(out, geminiPart{InlineData: &geminiBlob{
					MimeType: p.Image.MediaType, Data: p.Image.Data,
				}})
			}
		}
	}
	return out
}

func toolResponse(text string) json.RawMessage {
	// Gemini expects an object under functionResponse.response; a bare string is
	// wrapped so providers do not reject the shape.
	return mustJSON(map[string]string{"output": text})
}

// --- Gemini generateContent: decode a response -------------------------------

type geminiCandidate struct {
	Content struct {
		Role  string       `json:"role"`
		Parts []geminiPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

type geminiResponse struct {
	ResponseID    string            `json:"responseId"`
	ModelVersion  string            `json:"modelVersion"`
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		CachedContentTokens  int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// DecodeGeminiResponse maps a generateContent response to the pivot.
func DecodeGeminiResponse(body []byte) (*Response, error) {
	var wire geminiResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode gemini response: %w", err)
	}
	r := &Response{
		ID:           wire.ResponseID,
		Model:        wire.ModelVersion,
		FinishReason: geminiFinish(wire.Candidates, wire.PromptFeedback.BlockReason),
		Usage: Usage{
			PromptTokens:     wire.UsageMetadata.PromptTokenCount,
			CompletionTokens: wire.UsageMetadata.CandidatesTokenCount,
			CacheReadTokens:  wire.UsageMetadata.CachedContentTokens,
		},
	}
	if len(wire.Candidates) > 0 {
		var text strings.Builder
		for _, p := range wire.Candidates[0].Content.Parts {
			if p.Thought {
				r.Reasoning += p.Text
				continue
			}
			if p.FunctionCall != nil {
				r.ToolCalls = append(r.ToolCalls, ToolCall{
					ID:        "call_" + p.FunctionCall.Name,
					Name:      p.FunctionCall.Name,
					Arguments: compactJSON(p.FunctionCall.Args),
				})
				continue
			}
			text.WriteString(p.Text)
		}
		r.Content = text.String()
	}
	return r, nil
}

func geminiFinish(candidates []geminiCandidate, blockReason string) string {
	if blockReason != "" {
		return "content_filter"
	}
	if len(candidates) == 0 {
		return "stop"
	}
	for _, p := range candidates[0].Content.Parts {
		if p.FunctionCall != nil {
			return "tool_calls"
		}
	}
	switch candidates[0].FinishReason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

// --- stream: Gemini -> OpenAI ------------------------------------------------

func geminiToOpenAI(opts StreamOptions) Converter {
	return func(ctx context.Context, src io.Reader, dst io.Writer, flush func() error) error {
		r := newSSEReader(src)
		w := &sseWriter{w: dst, flush: flush}

		var (
			id       = "chatcmpl-stream"
			model    = opts.Model
			usage    = chunkUsage{}
			finish   = "stop"
			started  bool
			toolSeen int
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
			var chunkWire geminiResponse
			if err := json.Unmarshal([]byte(ev.Data), &chunkWire); err != nil {
				continue
			}
			if chunkWire.ResponseID != "" {
				id = chunkWire.ResponseID
			}
			if chunkWire.ModelVersion != "" {
				model = chunkWire.ModelVersion
			}
			if chunkWire.UsageMetadata.PromptTokenCount > 0 {
				usage.PromptTokens = chunkWire.UsageMetadata.PromptTokenCount
			}
			if chunkWire.UsageMetadata.CandidatesTokenCount > 0 {
				usage.CompletionTokens = chunkWire.UsageMetadata.CandidatesTokenCount
			}
			if !started {
				started = true
				if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
					return err
				}
			}
			if chunkWire.PromptFeedback.BlockReason != "" {
				finish = "content_filter"
			}
			if len(chunkWire.Candidates) == 0 {
				continue
			}
			cand := chunkWire.Candidates[0]
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					delta := chunkDelta{ToolCalls: []chunkTool{{
						Index: toolSeen, ID: "call_" + p.FunctionCall.Name, Type: "function",
					}}}
					delta.ToolCalls[0].Function.Name = p.FunctionCall.Name
					delta.ToolCalls[0].Function.Arguments = compactJSON(p.FunctionCall.Args)
					toolSeen++
					if err := w.event(newChunk(id, model, delta, nil)); err != nil {
						return err
					}
					continue
				}
				if p.Text == "" {
					continue
				}
				delta := chunkDelta{Content: p.Text}
				if p.Thought {
					delta = chunkDelta{ReasoningContent: p.Text}
				}
				if err := w.event(newChunk(id, model, delta, nil)); err != nil {
					return err
				}
			}
			if cand.FinishReason != "" {
				finish = geminiFinish([]geminiCandidate{cand}, "")
			}
		}

		if !started {
			if err := w.event(newChunk(id, model, chunkDelta{Role: "assistant"}, nil)); err != nil {
				return err
			}
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
