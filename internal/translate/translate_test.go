package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON %s: %v", b, err)
	}
	return m
}

func runConverter(t *testing.T, c Converter, input string) string {
	t.Helper()
	var out bytes.Buffer
	if err := c(context.Background(), strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("converter: %v", err)
	}
	return out.String()
}

const openAIRequestJSON = `{
  "model": "m1",
  "messages": [
    {"role": "system", "content": "be terse"},
    {"role": "user", "content": "hello"},
    {"role": "assistant", "content": null, "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_1", "content": "result text"}
  ],
  "max_tokens": 128,
  "temperature": 0.2,
  "stop": ["END"],
  "tools": [
    {"type": "function", "function": {"name": "lookup", "description": "search",
      "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}}
  ],
  "tool_choice": {"type": "function", "function": {"name": "lookup"}}
}`

func TestEncodeAnthropicRequest(t *testing.T) {
	req, err := DecodeOpenAI([]byte(openAIRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeAnthropic(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, body)

	if got["model"] != "m1" {
		t.Errorf("model = %v", got["model"])
	}
	if got["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if got["system"] != "be terse" {
		t.Errorf("system = %v", got["system"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant blocks = %v", blocks)
	}
	if blocks[0].(map[string]any)["id"] != "call_1" {
		t.Errorf("tool_use id = %v", blocks[0].(map[string]any)["id"])
	}
	toolResult := msgs[2].(map[string]any)
	trBlock := toolResult["content"].([]any)[0].(map[string]any)
	if trBlock["type"] != "tool_result" || trBlock["tool_use_id"] != "call_1" {
		t.Errorf("tool result block = %v", trBlock)
	}
	if trBlock["content"] != "result text" {
		t.Errorf("tool result content = %v", trBlock["content"])
	}
	choice := got["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "lookup" {
		t.Errorf("tool_choice = %v", choice)
	}
}

func TestEncodeAnthropicDefaultsMaxTokens(t *testing.T) {
	req, err := DecodeOpenAI([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeAnthropic(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, body)["max_tokens"]; got != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want the default", got)
	}
}

func TestDecodeAnthropicRequest(t *testing.T) {
	body := []byte(`{
	  "model": "claude",
	  "max_tokens": 64,
	  "system": [{"type":"text","text":"sys one"}],
	  "messages": [
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"calling"},
	      {"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"tu_1","content":"ok"},
	      {"type":"text","text":"continue"}
	    ]}
	  ],
	  "tools": [{"name":"lookup","description":"search","input_schema":{"type":"object"}}]
	}`)
	req, err := DecodeAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "sys one" || req.Model != "claude" || req.MaxTokens != 64 {
		t.Errorf("request = %+v", req)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 {
		t.Errorf("assistant = %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "tu_1" {
		t.Errorf("tool result = %+v", req.Messages[2])
	}
	if req.Messages[3].Role != "user" || req.Messages[3].Parts[0].Text != "continue" {
		t.Errorf("trailing user part = %+v", req.Messages[3])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Errorf("tools = %+v", req.Tools)
	}

	// And the pivot must be re-encodable as an OpenAI request.
	out, err := EncodeOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := decode(t, out)
	if wire["model"] != "claude" {
		t.Errorf("openai model = %v", wire["model"])
	}
}

func TestDecodeAnthropicResponse(t *testing.T) {
	body := []byte(`{
	  "id": "msg_1", "type": "message", "role": "assistant", "model": "claude",
	  "content": [
	    {"type":"thinking","thinking":"hmm"},
	    {"type":"text","text":"hi there"},
	    {"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 2}
	}`)
	r, err := DecodeAnthropicResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "hi there" || r.Reasoning != "hmm" {
		t.Errorf("content/reasoning = %q/%q", r.Content, r.Reasoning)
	}
	if r.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", r.FinishReason)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", r.ToolCalls)
	}
	if r.Usage.PromptTokens != 10 || r.Usage.CompletionTokens != 5 || r.Usage.CacheReadTokens != 2 {
		t.Errorf("usage = %+v", r.Usage)
	}

	out, err := EncodeOpenAI(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := decode(t, out)
	choice := wire["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi there" {
		t.Errorf("openai content = %v", msg["content"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	usage := wire["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
}

func TestEncodeAnthropicResponse(t *testing.T) {
	out, err := EncodeAnthropicResponse(&Response{
		ID: "chatcmpl-1", Model: "m", Content: "hello",
		ToolCalls:    []ToolCall{{ID: "c1", Name: "lookup", Arguments: `{"q":"x"}`}},
		FinishReason: "tool_calls",
		Usage:        Usage{PromptTokens: 3, CompletionTokens: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := decode(t, out)
	if wire["type"] != "message" || wire["role"] != "assistant" {
		t.Errorf("envelope = %v", wire)
	}
	if wire["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", wire["stop_reason"])
	}
	blocks := wire["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("content = %v", blocks)
	}
	if blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("tool block = %v", blocks[1])
	}
	usage := wire["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) {
		t.Errorf("usage = %v", usage)
	}
}

func TestAnthropicStreamToOpenAI(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":7,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"lookup","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	converter, ok := NewStreamConverter(FormatAnthropic, StreamOptions{Model: "requested"})
	if !ok {
		t.Fatal("no anthropic stream converter")
	}
	out := runConverter(t, converter, stream)

	if !strings.Contains(out, `"content":"Hel"`) {
		t.Errorf("text delta missing:\n%s", out)
	}
	if !strings.Contains(out, `"name":"lookup"`) {
		t.Errorf("tool call start missing:\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"q\":"`) {
		t.Errorf("tool argument delta missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish reason missing:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("stream does not end with [DONE]:\n%s", out)
	}
}

func TestOpenAIStreamToAnthropic(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out := runConverter(t, AnthropicIngressConverter("m"), stream)

	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"type":"tool_use"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGeminiRequestAndResponse(t *testing.T) {
	req, err := DecodeOpenAI([]byte(openAIRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeGemini(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := decode(t, body)
	if wire["systemInstruction"] == nil {
		t.Error("system instruction is missing")
	}
	contents := wire["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %v", contents)
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Errorf("assistant role = %v", contents[1].(map[string]any)["role"])
	}

	respBody := []byte(`{
	  "responseId": "r1", "modelVersion": "gemini-2.5-pro",
	  "candidates": [{"content":{"role":"model","parts":[
	    {"text":"hello"},
	    {"functionCall":{"name":"lookup","args":{"q":"x"}}}
	  ]},"finishReason":"STOP"}],
	  "usageMetadata": {"promptTokenCount": 4, "candidatesTokenCount": 6}
	}`)
	resp, err := DecodeGeminiResponse(respBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || len(resp.ToolCalls) != 1 {
		t.Errorf("response = %+v", resp)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 4 || resp.Usage.CompletionTokens != 6 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestGeminiStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"responseId":"r1","modelVersion":"gemini","candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]}}]}`,
		``,
		`data: {"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}`,
		``,
	}, "\n")

	converter, ok := NewStreamConverter(FormatGemini, StreamOptions{Model: "gemini"})
	if !ok {
		t.Fatal("no gemini stream converter")
	}
	out := runConverter(t, converter, stream)
	for _, want := range []string{`"content":"Hi"`, `"name":"lookup"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestResponsesRequestAndResponse(t *testing.T) {
	req, err := DecodeOpenAI([]byte(openAIRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeResponses(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := decode(t, body)
	if wire["instructions"] != "be terse" {
		t.Errorf("instructions = %v", wire["instructions"])
	}
	input := wire["input"].([]any)
	// The system prompt becomes "instructions"; the assistant turn had no text,
	// so its only input item is the function call.
	if len(input) != 3 {
		t.Fatalf("input = %v", input)
	}
	if input[0].(map[string]any)["type"] != "message" {
		t.Errorf("user message item = %v", input[0])
	}
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Errorf("assistant tool call item = %v", input[1])
	}
	if input[2].(map[string]any)["type"] != "function_call_output" {
		t.Errorf("tool output item = %v", input[2])
	}

	respBody := []byte(`{
	  "id":"resp_1","model":"m","status":"completed",
	  "output":[
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
	    {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}
	  ],
	  "usage":{"input_tokens":3,"output_tokens":4}
	}`)
	resp, err := DecodeResponsesResponse(respBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" || len(resp.ToolCalls) != 1 || resp.FinishReason != "tool_calls" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 4 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestResponsesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"q\":"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":4}}}`,
		``,
	}, "\n")

	converter, ok := NewStreamConverter(FormatResponses, StreamOptions{Model: "m"})
	if !ok {
		t.Fatal("no responses stream converter")
	}
	out := runConverter(t, converter, stream)
	for _, want := range []string{`"content":"Hi"`, `"name":"lookup"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
