package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	"github.com/tidwall/gjson"
)

func translateNonStreamResponse(ctx context.Context, cfg config.Config, req pluginapi.ExecutorRequest, built builtRequest, payload []byte) ([]byte, error) {
	native := rewriteResponseModel(payload, built.publicModel, built.forceMapping)
	from, to := responseFormats(cfg, req)
	if isImageRequest(req) || to == "" || from == to {
		return native, nil
	}
	from = translatorResponseSource(from)
	if !sdktranslator.HasNonStreamResponseTransformer(to, from) {
		return nil, fmt.Errorf("response translation from %q to %q is unavailable", from, to)
	}
	translationPayload := native
	if cfg.Protocol == config.ProtocolOpenAIResponses && gjson.GetBytes(native, "object").String() == "response" {
		translationPayload = append([]byte(`{"type":"response.completed","response":`), native...)
		translationPayload = append(translationPayload, '}')
	}
	originalRequest := req.OriginalRequest
	if len(originalRequest) == 0 {
		originalRequest = req.Payload
	}
	translated := sdktranslator.TranslateNonStream(ctx, from, to, req.Model, originalRequest, req.Payload, translationPayload, nil)
	if translated == nil {
		return nil, fmt.Errorf("response translation from %q to %q returned no payload", from, to)
	}
	return rewriteResponseModel(translated, built.publicModel, built.forceMapping), nil
}

func translateStreamResponse(ctx context.Context, cfg config.Config, req pluginapi.ExecutorRequest, built builtRequest, input <-chan pluginapi.ExecutorStreamChunk) <-chan pluginapi.ExecutorStreamChunk {
	from, to := responseFormats(cfg, req)
	if isImageRequest(req) || to == "" || from == to {
		return input
	}
	from = translatorResponseSource(from)
	output := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(output)
		if !sdktranslator.HasStreamResponseTransformer(to, from) {
			sendExecutorChunk(ctx, output, pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("stream response translation from %q to %q is unavailable", from, to)})
			return
		}
		originalRequest := req.OriginalRequest
		if len(originalRequest) == 0 {
			originalRequest = req.Payload
		}
		var state any
		translate := func(payload []byte) bool {
			frames := sdktranslator.TranslateStream(ctx, from, to, req.Model, originalRequest, req.Payload, payload, &state)
			for _, frame := range frames {
				mapped := rewriteStreamPayload(frame, built.publicModel, built.forceMapping)
				if !sendExecutorChunk(ctx, output, pluginapi.ExecutorStreamChunk{Payload: mapped}) {
					return false
				}
			}
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-input:
				if !ok {
					if from == sdktranslator.FormatOpenAI {
						translate([]byte("data: [DONE]"))
					}
					return
				}
				if chunk.Err != nil {
					if !sendExecutorChunk(ctx, output, chunk) {
						return
					}
					continue
				}
				if !translate(chunk.Payload) {
					return
				}
			}
		}
	}()
	return output
}

func validateStreamResponseTranslation(cfg config.Config, req pluginapi.ExecutorRequest) error {
	from, to := responseFormats(cfg, req)
	if isImageRequest(req) || to == "" || from == to {
		return nil
	}
	from = translatorResponseSource(from)
	if !sdktranslator.HasStreamResponseTransformer(to, from) {
		return fmt.Errorf("stream response translation from %q to %q is unavailable", from, to)
	}
	return nil
}

func sendExecutorChunk(ctx context.Context, output chan<- pluginapi.ExecutorStreamChunk, chunk pluginapi.ExecutorStreamChunk) bool {
	select {
	case output <- pluginapi.ExecutorStreamChunk{Payload: bytes.Clone(chunk.Payload), Err: chunk.Err}:
		return true
	case <-ctx.Done():
		return false
	}
}

func responseFormats(cfg config.Config, req pluginapi.ExecutorRequest) (sdktranslator.Format, sdktranslator.Format) {
	from := normalizeResponseFormat(cfg.Protocol.ExecutorFormat())
	to := normalizeResponseFormat(req.Format)
	if to == "" {
		to = from
	}
	return from, to
}

func translatorResponseSource(format sdktranslator.Format) sdktranslator.Format {
	// CPA's built-in response converters use "codex" as the canonical source
	// for native OpenAI Responses payloads; "openai-response" is the client
	// target schema used by request routing.
	if format == sdktranslator.FormatOpenAIResponse {
		return sdktranslator.FormatCodex
	}
	return format
}

func normalizeResponseFormat(value string) sdktranslator.Format {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none":
		return ""
	case "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions":
		return sdktranslator.FormatOpenAI
	case "responses", "openai-responses", "openai_responses":
		return sdktranslator.FormatOpenAIResponse
	case "anthropic", "anthropic-messages":
		return sdktranslator.FormatClaude
	default:
		return sdktranslator.FromString(strings.TrimSpace(value))
	}
}

func tokenCountPayload(format sdktranslator.Format, count int64) ([]byte, error) {
	var payload any
	switch normalizeResponseFormat(format.String()) {
	case sdktranslator.FormatClaude:
		payload = map[string]int64{"input_tokens": count}
	case sdktranslator.FormatGemini:
		payload = map[string]int64{"totalTokens": count}
	case sdktranslator.FormatOpenAIResponse:
		payload = map[string]any{"usage": map[string]int64{"input_tokens": count, "output_tokens": 0, "total_tokens": count}}
	case sdktranslator.FormatOpenAI:
		payload = map[string]any{"usage": map[string]int64{"prompt_tokens": count, "completion_tokens": 0, "total_tokens": count}}
	default:
		return nil, fmt.Errorf("token count output format %q is unsupported", format)
	}
	return json.Marshal(payload)
}

func tokenCountFromPayload(protocol config.Protocol, payload []byte) (int64, error) {
	root := gjson.ParseBytes(payload)
	var paths []string
	switch protocol {
	case config.ProtocolAnthropic:
		paths = []string{"input_tokens", "usage.input_tokens"}
	case config.ProtocolGemini:
		paths = []string{"totalTokens", "total_tokens", "usageMetadata.promptTokenCount", "usage_metadata.prompt_token_count"}
	default:
		paths = []string{"usage.input_tokens", "usage.prompt_tokens", "input_tokens", "totalTokens"}
	}
	for _, path := range paths {
		value := root.Get(path)
		if value.Exists() && value.Type == gjson.Number {
			return value.Int(), nil
		}
	}
	return 0, fmt.Errorf("provider token count response does not contain a token count")
}
