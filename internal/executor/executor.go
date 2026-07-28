package executor

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Executor struct {
	cfg         config.Config
	httpClients hostHTTPClientFactory
}

func New(cfg config.Config) *Executor {
	cfg.Normalize()
	return &Executor{cfg: cfg, httpClients: newSafeHTTPClient}
}

func NewExecutor(cfg config.Config) *Executor { return New(cfg) }

func (e *Executor) Identifier() string { return provider.ID }

func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	built, errBuild := buildModelRequest(ctx, e.cfg, req, false, "generate")
	if errBuild != nil {
		return pluginapi.ExecutorResponse{}, errBuild
	}
	client, errClient := e.upstreamClient(built.proxyURL)
	if errClient != nil {
		return pluginapi.ExecutorResponse{}, errClient
	}
	response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
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
	payload, errTranslate := translateNonStreamResponse(ctx, e.cfg, req, built, response.Body)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, errTranslate
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: response.Headers}, nil
}

func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	built, errBuild := buildModelRequest(ctx, e.cfg, req, true, "generate")
	if errBuild != nil {
		return pluginapi.ExecutorStreamResponse{}, errBuild
	}
	if errTranslation := validateStreamResponseTranslation(e.cfg, req); errTranslation != nil {
		return pluginapi.ExecutorStreamResponse{}, errTranslation
	}
	client, errClient := e.upstreamClient(built.proxyURL)
	if errClient != nil {
		return pluginapi.ExecutorStreamResponse{}, errClient
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	response, errDo := client.DoStream(streamCtx, pluginapi.HTTPRequest{
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
	nativeChunks := convertHTTPChunksWithClose(
		streamCtx,
		response.Chunks,
		built.publicModel,
		built.forceMapping,
		e.cfg.Protocol == config.ProtocolGemini,
		cancelStream,
	)
	return pluginapi.ExecutorStreamResponse{
		Headers: response.Headers,
		Chunks:  translateStreamResponse(ctx, e.cfg, req, built, nativeChunks),
	}, nil
}

func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if e.cfg.Protocol != config.ProtocolAnthropic && e.cfg.Protocol != config.ProtocolGemini {
		if _, err := apiKeyFromStorageForConfig(req.StorageJSON, e.cfg); err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		return localTokenCount(req.Payload, responseFormatForCount(e.cfg, req))
	}
	built, errBuild := buildModelRequest(ctx, e.cfg, req, false, "countTokens")
	if errBuild != nil {
		return pluginapi.ExecutorResponse{}, errBuild
	}
	client, errClient := e.upstreamClient(built.proxyURL)
	if errClient != nil {
		return pluginapi.ExecutorResponse{}, errClient
	}
	response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
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
	count, errCount := tokenCountFromPayload(e.cfg.Protocol, response.Body)
	if errCount != nil {
		return pluginapi.ExecutorResponse{}, errCount
	}
	payload, errPayload := tokenCountPayload(normalizeResponseFormat(responseFormatForCount(e.cfg, req)), count)
	if errPayload != nil {
		return pluginapi.ExecutorResponse{}, errPayload
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: response.Headers}, nil
}

func localTokenCount(payload []byte, format string) (pluginapi.ExecutorResponse, error) {
	// Compatibility endpoints rarely expose token counting. This deterministic
	// estimate keeps the required executor capability usable without another call.
	count := int64((utf8.RuneCount(payload) + 3) / 4)
	body, err := tokenCountPayload(normalizeResponseFormat(format), count)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return pluginapi.ExecutorResponse{Payload: body}, nil
}

func responseFormatForCount(cfg config.Config, req pluginapi.ExecutorRequest) string {
	_, format := responseFormats(cfg, req)
	return format.String()
}

func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if errURL := validateExecutorURL(e.cfg.BaseURL, req.URL); errURL != nil {
		return pluginapi.ExecutorHTTPResponse{}, errURL
	}
	requestURL, errURL := stripAuthenticationQuery(req.URL)
	if errURL != nil {
		return pluginapi.ExecutorHTTPResponse{}, errURL
	}
	credential, errKey := credentialFromStorageForConfig(req.StorageJSON, e.cfg)
	if errKey != nil {
		return pluginapi.ExecutorHTTPResponse{}, errKey
	}
	apiKey := credential.apiKey
	client, errClient := e.upstreamClient(credential.proxyURL)
	if errClient != nil {
		return pluginapi.ExecutorHTTPResponse{}, errClient
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
	response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
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

func (e *Executor) upstreamClient(proxyURL string) (pluginapi.HostHTTPClient, error) {
	factory := e.httpClients
	if factory == nil {
		factory = newSafeHTTPClient
	}
	client, err := factory(proxyURL)
	if err != nil {
		return nil, statusError{statusCode: http.StatusServiceUnavailable, body: []byte("provider proxy configuration is invalid")}
	}
	return client, nil
}
