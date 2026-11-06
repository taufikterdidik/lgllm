package translate

import (
	"fmt"
)

// Format names match registry.Format*.
const (
	FormatChat      = "openai"
	FormatAnthropic = "anthropic"
	FormatGemini    = "gemini"
	FormatResponses = "openai-responses"
	FormatOllama    = "ollama"
)

// EncodeRequest renders a pivot request in the format a provider expects.
func EncodeRequest(format string, req *Request) ([]byte, error) {
	switch format {
	case FormatChat:
		return EncodeOpenAIRequest(req)
	case FormatAnthropic:
		return EncodeAnthropic(req)
	case FormatGemini:
		return EncodeGemini(req)
	case FormatResponses:
		return EncodeResponses(req)
	default:
		return nil, fmt.Errorf("format %q has no request translator", format)
	}
}

// DecodeResponse parses a non-streamed provider response into the pivot.
func DecodeResponse(format string, body []byte) (*Response, error) {
	switch format {
	case FormatChat:
		return DecodeOpenAIResponse(body)
	case FormatAnthropic:
		return DecodeAnthropicResponse(body)
	case FormatGemini:
		return DecodeGeminiResponse(body)
	case FormatResponses:
		return DecodeResponsesResponse(body)
	default:
		return nil, fmt.Errorf("format %q has no response translator", format)
	}
}
