package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestTranslateNonStreamResponseAllProtocolDirections(t *testing.T) {
	native := map[config.Protocol][]byte{
		config.ProtocolOpenAIChat:      []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		config.ProtocolOpenAIResponses: []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"upstream-model","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`),
		config.ProtocolAnthropic:       []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`),
		config.ProtocolGemini:          []byte(`{"responseId":"resp_1","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`),
	}
	targets := []string{"openai", "openai-response", "claude", "gemini"}
	for protocol, payload := range native {
		for _, target := range targets {
			name := fmt.Sprintf("%s to %s", protocol, target)
			t.Run(name, func(t *testing.T) {
				cfg := testConfig("https://api.example.com/v1", protocol, false)
				req := pluginapi.ExecutorRequest{
					Model:           "public-model",
					Format:          target,
					OriginalRequest: []byte(`{"model":"public-model","messages":[],"stream":false}`),
					Payload:         []byte(`{"model":"upstream-model","messages":[]}`),
				}
				got, err := translateNonStreamResponse(context.Background(), cfg, req, builtRequest{}, payload)
				if err != nil {
					t.Fatalf("translateNonStreamResponse() error = %v", err)
				}
				if target == protocol.ExecutorFormat() {
					if !bytes.Equal(got, payload) {
						t.Fatalf("same-format response changed:\n got %s\nwant %s", got, payload)
					}
					return
				}
				assertResponseFormat(t, target, got)
			})
		}
	}
}

func TestTranslateStreamResponseCrossProtocolDirections(t *testing.T) {
	tests := []struct {
		name       string
		protocol   config.Protocol
		target     string
		chunks     []string
		wantMarker string
	}{
		{
			name: "OpenAI to Claude", protocol: config.ProtocolOpenAIChat, target: "claude",
			chunks:     []string{`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
			wantMarker: "message_start",
		},
		{
			name: "Claude to OpenAI", protocol: config.ProtocolAnthropic, target: "openai",
			chunks:     []string{`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`},
			wantMarker: "chat.completion.chunk",
		},
		{
			name: "Gemini to Responses", protocol: config.ProtocolGemini, target: "openai-response",
			chunks:     []string{`{"responseId":"resp_1","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`},
			wantMarker: "response.",
		},
		{
			name: "Responses to Gemini", protocol: config.ProtocolOpenAIResponses, target: "gemini",
			chunks:     []string{`data: {"type":"response.output_text.delta","response_id":"resp_1","output_index":0,"content_index":0,"delta":"ok"}`},
			wantMarker: "candidates",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := make(chan pluginapi.ExecutorStreamChunk, len(test.chunks))
			for _, payload := range test.chunks {
				input <- pluginapi.ExecutorStreamChunk{Payload: []byte(payload)}
			}
			close(input)
			cfg := testConfig("https://api.example.com/v1", test.protocol, false)
			req := pluginapi.ExecutorRequest{
				Model:           "public-model",
				Format:          test.target,
				OriginalRequest: []byte(`{"model":"public-model","messages":[],"stream":true}`),
				Payload:         []byte(`{"model":"upstream-model","messages":[],"stream":true}`),
			}
			var output bytes.Buffer
			for chunk := range translateStreamResponse(context.Background(), cfg, req, builtRequest{}, input) {
				if chunk.Err != nil {
					t.Fatalf("translated stream error = %v", chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), test.wantMarker) {
				t.Fatalf("translated stream = %q, want marker %q", output.String(), test.wantMarker)
			}
		})
	}
}

func TestTranslateStreamResponseSameFormatPassthrough(t *testing.T) {
	payload := []byte("data: { \"model\" : \"native\" }\n\n")
	input := make(chan pluginapi.ExecutorStreamChunk, 1)
	input <- pluginapi.ExecutorStreamChunk{Payload: payload}
	close(input)
	req := pluginapi.ExecutorRequest{Format: "openai"}
	chunks := translateStreamResponse(context.Background(), testConfig("https://api.example.com/v1", config.ProtocolOpenAIChat, false), req, builtRequest{}, input)
	chunk, ok := <-chunks
	if !ok || chunk.Err != nil || !bytes.Equal(chunk.Payload, payload) {
		t.Fatalf("same-format stream chunk = %#v", chunk)
	}
}

func TestValidateStreamResponseTranslationRejectsUnavailableTarget(t *testing.T) {
	cfg := testConfig("https://api.example.com/v1", config.ProtocolOpenAIChat, false)
	err := validateStreamResponseTranslation(cfg, pluginapi.ExecutorRequest{Format: "unsupported-test-format"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("validateStreamResponseTranslation() error = %v", err)
	}
}

func TestCountTokensReturnsRequestedProtocolSchema(t *testing.T) {
	cfg := testConfig("https://api.example.com/v1", config.ProtocolOpenAIChat, false)
	count := int64((len([]byte(`{"messages":[]}`)) + 3) / 4)
	tests := []struct {
		format string
		want   map[string]any
	}{
		{"openai", map[string]any{"usage": map[string]any{"prompt_tokens": float64(count), "completion_tokens": float64(0), "total_tokens": float64(count)}}},
		{"openai-response", map[string]any{"usage": map[string]any{"input_tokens": float64(count), "output_tokens": float64(0), "total_tokens": float64(count)}}},
		{"claude", map[string]any{"input_tokens": float64(count)}},
		{"gemini", map[string]any{"totalTokens": float64(count)}},
	}
	for _, test := range tests {
		t.Run(test.format, func(t *testing.T) {
			response, err := New(cfg).CountTokens(context.Background(), pluginapi.ExecutorRequest{
				Format: test.format, Payload: []byte(`{"messages":[]}`), StorageJSON: credentialJSON(cfg, "secret"),
			})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(response.Payload, &got); err != nil {
				t.Fatalf("CountTokens() payload = %s: %v", response.Payload, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("CountTokens() schema = %#v, want %#v", got, test.want)
			}
		})
	}
}

func assertResponseFormat(t *testing.T, format string, payload []byte) {
	t.Helper()
	switch format {
	case "openai":
		if gjson.GetBytes(payload, "object").String() != "chat.completion" || !gjson.GetBytes(payload, "choices").IsArray() {
			t.Fatalf("OpenAI response = %s", payload)
		}
	case "openai-response":
		if gjson.GetBytes(payload, "object").String() != "response" || !gjson.GetBytes(payload, "output").IsArray() {
			t.Fatalf("Responses response = %s", payload)
		}
	case "claude":
		if gjson.GetBytes(payload, "type").String() != "message" || !gjson.GetBytes(payload, "content").IsArray() {
			t.Fatalf("Claude response = %s", payload)
		}
	case "gemini":
		if !gjson.GetBytes(payload, "candidates").IsArray() {
			t.Fatalf("Gemini response = %s", payload)
		}
	}
}
