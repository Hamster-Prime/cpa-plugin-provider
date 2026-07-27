package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Executor struct {
	cfg config.Config
}

func New(cfg config.Config) *Executor {
	cfg.Normalize()
	return &Executor{cfg: cfg}
}

func NewExecutor(cfg config.Config) *Executor { return New(cfg) }

func (e *Executor) Identifier() string { return provider.ID }

func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	built, errBuild := buildModelRequest(e.cfg, req, false, "generate")
	if errBuild != nil {
		return pluginapi.ExecutorResponse{}, errBuild
	}
	if req.HTTPClient == nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("host HTTP client is required")
	}
	response, errDo := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  built.method,
		URL:     built.url,
		Headers: built.headers,
		Body:    built.body,
	})
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, newUpstreamStatusError(response.StatusCode, response.Body, built.sensitiveValues...)
	}
	payload := rewriteResponseModel(response.Body, built.publicModel, built.forceMapping)
	return pluginapi.ExecutorResponse{Payload: payload, Headers: response.Headers}, nil
}

func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	built, errBuild := buildModelRequest(e.cfg, req, true, "generate")
	if errBuild != nil {
		return pluginapi.ExecutorStreamResponse{}, errBuild
	}
	if req.HTTPClient == nil {
		return pluginapi.ExecutorStreamResponse{}, fmt.Errorf("host HTTP client is required")
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	response, errDo := req.HTTPClient.DoStream(streamCtx, pluginapi.HTTPRequest{
		Method:  built.method,
		URL:     built.url,
		Headers: built.headers,
		Body:    built.body,
	})
	if errDo != nil {
		cancelStream()
		return pluginapi.ExecutorStreamResponse{}, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := readStreamErrorBody(streamCtx, response.Chunks)
		cancelStream()
		return pluginapi.ExecutorStreamResponse{}, newUpstreamStatusError(response.StatusCode, body, built.sensitiveValues...)
	}
	return pluginapi.ExecutorStreamResponse{
		Headers: response.Headers,
		Chunks: convertHTTPChunksWithClose(
			streamCtx,
			response.Chunks,
			built.publicModel,
			built.forceMapping,
			e.cfg.Protocol == config.ProtocolGemini,
			cancelStream,
		),
	}, nil
}

func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if e.cfg.Protocol != config.ProtocolAnthropic && e.cfg.Protocol != config.ProtocolGemini {
		if _, err := apiKeyFromStorageForConfig(req.StorageJSON, e.cfg); err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		return localTokenCount(req.Payload, e.cfg.Protocol), nil
	}
	built, errBuild := buildModelRequest(e.cfg, req, false, "countTokens")
	if errBuild != nil {
		return pluginapi.ExecutorResponse{}, errBuild
	}
	if req.HTTPClient == nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("host HTTP client is required")
	}
	response, errDo := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  built.method,
		URL:     built.url,
		Headers: built.headers,
		Body:    built.body,
	})
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, newUpstreamStatusError(response.StatusCode, response.Body, built.sensitiveValues...)
	}
	return pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers}, nil
}

func localTokenCount(payload []byte, protocol config.Protocol) pluginapi.ExecutorResponse {
	// Compatibility endpoints rarely expose token counting. This deterministic
	// estimate keeps the required executor capability usable without another call.
	count := (utf8.RuneCount(payload) + 3) / 4
	var body []byte
	if protocol == config.ProtocolOpenAIResponses {
		body, _ = json.Marshal(map[string]any{"usage": map[string]int{"input_tokens": count, "output_tokens": 0, "total_tokens": count}})
	} else {
		body, _ = json.Marshal(map[string]any{"usage": map[string]int{"prompt_tokens": count, "completion_tokens": 0, "total_tokens": count}})
	}
	return pluginapi.ExecutorResponse{Payload: body}
}

func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("host HTTP client is required")
	}
	if errURL := validateExecutorURL(e.cfg.BaseURL, req.URL); errURL != nil {
		return pluginapi.ExecutorHTTPResponse{}, errURL
	}
	requestURL, errURL := stripAuthenticationQuery(req.URL)
	if errURL != nil {
		return pluginapi.ExecutorHTTPResponse{}, errURL
	}
	apiKey, errKey := apiKeyFromStorageForConfig(req.StorageJSON, e.cfg)
	if errKey != nil {
		return pluginapi.ExecutorHTTPResponse{}, errKey
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodPost
	}
	headers := req.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	applyAuthentication(headers, e.cfg.Protocol, apiKey, false)
	for key, value := range e.cfg.Headers {
		if strings.TrimSpace(key) != "" {
			headers.Set(key, value)
		}
	}
	response, errDo := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  method,
		URL:     requestURL,
		Headers: headers,
		Body:    append([]byte(nil), req.Body...),
	})
	if errDo != nil {
		return pluginapi.ExecutorHTTPResponse{}, errDo
	}
	body := response.Body
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body = redactSensitiveValues(body, upstreamSensitiveValues(apiKey, e.cfg.Headers)...)
	}
	return pluginapi.ExecutorHTTPResponse{
		StatusCode: response.StatusCode,
		Headers:    response.Headers,
		Body:       body,
	}, nil
}
