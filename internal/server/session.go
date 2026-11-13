package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lgllm/internal/sessionid"
)

// sessionHeaderNames are the inbound headers that can identify a conversation.
//
// X-Lgllm-Session is lgllm's explicit override. x-opencode-session is the
// header the Go gateway requires; the rest are native session headers emitted
// by coding agents. x-session-affinity is accepted as a source even though the
// Go gateway rejects it on its own: the value is re-emitted under the
// provider's configured header.
var sessionHeaderNames = []string{
	"X-Lgllm-Session",
	"X-Opencode-Session",
	"X-Opencode-Session-Id",
	"X-Session-Id",
	"X-Session-Affinity",
	"Session-Id",
	"Session_id",
	"Conversation-Id",
	"X-Conversation-Id",
	"X-Claude-Code-Session-Id",
}

// explicitSessionID returns the first usable client-supplied conversation id.
func explicitSessionID(r *http.Request) string {
	for _, name := range sessionHeaderNames {
		if v := sessionid.Sanitize(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// sessionKey finds the conversation identifier used for sticky credentials.
//
// It never invents an id: a combo with a round-robin strategy must keep
// rotating when the client supplied nothing. conversationSession handles the
// outbound header separately and may fall back to a deterministic fingerprint.
func sessionKey(r *http.Request, payload map[string]json.RawMessage) string {
	if v := explicitSessionID(r); v != "" {
		return v
	}
	if v := sessionid.Sanitize(rawString(payload["prompt_cache_key"])); v != "" {
		return v
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if raw, ok := payload["metadata"]; ok {
		_ = json.Unmarshal(raw, &meta)
		if v := sessionid.Sanitize(meta.UserID); v != "" {
			return v
		}
	}
	return sessionid.Sanitize(rawString(payload["user"]))
}

// conversationSession returns the id sent in a provider's session header.
//
// A client-supplied id always wins. When there is none, the first user message
// is hashed into a deterministic id: agents resend the conversation on every
// turn, so the seed stays stable while unrelated conversations get different
// ids. The request id is only a last resort for a body with no prompt text.
func conversationSession(r *http.Request, payload map[string]json.RawMessage, requestID, clientKey string) string {
	if v := explicitSessionID(r); v != "" {
		return v
	}
	if v := sessionid.Sanitize(rawString(payload["prompt_cache_key"])); v != "" {
		return v
	}
	if seed := conversationSeed(payload); seed != "" {
		return sessionid.Fingerprint(clientKey + "\x00" + seed)
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if raw, ok := payload["metadata"]; ok {
		_ = json.Unmarshal(raw, &meta)
		if v := sessionid.Sanitize(meta.UserID); v != "" {
			return v
		}
	}
	if v := sessionid.Sanitize(rawString(payload["user"])); v != "" {
		return v
	}
	if requestID != "" {
		return sessionid.Fingerprint("request:" + requestID)
	}
	return ""
}

// conversationSeed picks the stable part of a request body. The first user
// message is preferred because system prompts often contain changing context
// (dates, working directory) even within one conversation.
func conversationSeed(payload map[string]json.RawMessage) string {
	if v := firstUserText(payload["messages"]); v != "" {
		return v
	}
	if v := firstUserText(payload["input"]); v != "" {
		return v
	}
	if v := textValue(payload["system"]); v != "" {
		return v
	}
	if v := textValue(payload["instructions"]); v != "" {
		return v
	}
	return textValue(payload["prompt"])
}

// firstUserText walks a messages/input array and returns the first user text.
func firstUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	for _, item := range items {
		var member map[string]json.RawMessage
		if json.Unmarshal(item, &member) != nil {
			continue
		}
		if rawString(member["role"]) != "user" {
			continue
		}
		if text := textValue(member["content"]); text != "" {
			return text
		}
	}
	return ""
}

// textValue flattens the string, string-array, and content-part shapes used by
// the OpenAI, Anthropic and Responses dialects.
func textValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.TrimSpace(strings.Join(list, ""))
	}
	var parts []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Text != "" {
				b.WriteString(part.Text)
			}
			if part.Content != "" {
				b.WriteString(part.Content)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}
