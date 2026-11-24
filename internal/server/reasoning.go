package server

import (
	"encoding/json"

	"lgllm/internal/registry"
)

// applyReasoningEffort overwrites the reasoning hint in a rendered provider
// body. Only OpenAI Chat and Responses have a direct equivalent; Anthropic
// thinking budgets and Gemini thinking configuration are not one-to-one, so
// they are left untouched.
func applyReasoningEffort(body []byte, format, effort string) ([]byte, error) {
	switch format {
	case registry.FormatChat:
		return setTopLevelReasoningEffort(body, effort)
	case registry.FormatResponses:
		return setResponsesReasoningEffort(body, effort)
	default:
		return body, nil
	}
}

func setTopLevelReasoningEffort(body []byte, effort string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(effort)
	if err != nil {
		return nil, err
	}
	doc["reasoning_effort"] = raw
	return json.Marshal(doc)
}

// setResponsesReasoningEffort replaces only the nested effort, preserving other
// members such as reasoning.summary. A client that sent a non-object reasoning
// value gets a fresh object rather than an error.
func setResponsesReasoningEffort(body []byte, effort string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	reasoning := make(map[string]json.RawMessage)
	if raw, ok := doc["reasoning"]; ok {
		_ = json.Unmarshal(raw, &reasoning)
		if reasoning == nil {
			reasoning = make(map[string]json.RawMessage)
		}
	}
	raw, err := json.Marshal(effort)
	if err != nil {
		return nil, err
	}
	reasoning["effort"] = raw
	encoded, err := json.Marshal(reasoning)
	if err != nil {
		return nil, err
	}
	doc["reasoning"] = encoded
	return json.Marshal(doc)
}
