package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type capturedRequest struct {
	path    string
	query   url.Values
	headers http.Header
	body    []byte
}

func TestExecuteRoutesAllProtocols(t *testing.T) {
	tests := []struct {
		name          string
		protocol      config.Protocol
		wantPath      string
		wantAuthName  string
		wantAuthValue string
	}{
		{name: "openai chat completions", protocol: config.ProtocolOpenAIChat, wantPath: "/v1/chat/completions", wantAuthName: "Authorization", wantAuthValue: "Bearer secret"},
		{name: "openai responses", protocol: config.ProtocolOpenAIResponses, wantPath: "/v1/responses", wantAuthName: "Authorization", wantAuthValue: "Bearer secret"},
		{name: "anthropic messages", protocol: config.ProtocolAnthropic, wantPath: "/v1/messages", wantAuthName: "X-Api-Key", wantAuthValue: "secret"},
		{name: "gemini", protocol: config.ProtocolGemini, wantPath: "/v1/models/upstream-model:generateContent", wantAuthName: "X-Goog-Api-Key", wantAuthValue: "secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan capturedRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				requests <- capturedRequest{
					path:    request.URL.Path,
					query:   request.URL.Query(),
					headers: request.Header.Clone(),
					body:    body,
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"model":"upstream-model","ok":true}`))
			}))
			defer server.Close()

			cfg := testConfig(server.URL+"/v1", test.protocol, false)
			result, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
				Model:       "public-model(high)",
				Payload:     []byte(`{"model":"public-model","messages":[],"stream":true}`),
				Headers:     http.Header{"Authorization": []string{"Bearer caller"}},
				Metadata:    map[string]any{requestedModelMetaKey: "team/public-model(high)"},
				StorageJSON: credentialJSON(cfg, "secret"),
				HTTPClient:  &networkHostClient{},
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			request := <-requests
			if request.path != test.wantPath {
				t.Fatalf("path = %q, want %q", request.path, test.wantPath)
			}
			if got := request.headers.Get(test.wantAuthName); got != test.wantAuthValue {
				t.Fatalf("%s = %q, want %q", test.wantAuthName, got, test.wantAuthValue)
			}
			if got := request.headers.Get("X-Custom"); got != "configured" {
				t.Fatalf("X-Custom = %q, want configured", got)
			}
			if test.protocol == config.ProtocolAnthropic && request.headers.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatalf("Anthropic-Version = %q", request.headers.Get("Anthropic-Version"))
			}
			if test.protocol == config.ProtocolGemini {
				if gjson.GetBytes(request.body, "model").Exists() {
					t.Fatalf("Gemini body contains endpoint-only model: %s", request.body)
				}
			} else if got := gjson.GetBytes(request.body, "model").String(); got != "upstream-model" {
				t.Fatalf("upstream model = %q, want upstream-model: %s", got, request.body)
			}
			if gjson.GetBytes(request.body, "stream").Exists() {
				t.Fatalf("non-stream body retained stream: %s", request.body)
			}
			thinkingPath := map[config.Protocol]string{
				config.ProtocolOpenAIChat:      "reasoning_effort",
				config.ProtocolOpenAIResponses: "reasoning.effort",
				config.ProtocolAnthropic:       "output_config.effort",
				config.ProtocolGemini:          "generationConfig.thinkingConfig.thinkingLevel",
			}[test.protocol]
			if got := gjson.GetBytes(request.body, thinkingPath).String(); got != "high" {
				t.Fatalf("requested_model thinking field %s = %q, want high: %s", thinkingPath, got, request.body)
			}
			if got := gjson.GetBytes(result.Payload, "model").String(); got != "team/public-model" {
				t.Fatalf("force-mapped response model = %q: %s", got, result.Payload)
			}
		})
	}
}

func TestExecuteResponsesCompactUsesCompactEndpoint(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := captureServer(requests, []byte(`{"id":"compact"}`))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolOpenAIResponses, false)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model(8192)",
		Alt:         "responses/compact",
		Payload:     []byte(`{"model":"public-model","stream":true}`),
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	request := <-requests
	if request.path != "/v1/responses/compact" {
		t.Fatalf("path = %q, want /v1/responses/compact", request.path)
	}
	if gjson.GetBytes(request.body, "stream").Exists() {
		t.Fatalf("compact body retained stream: %s", request.body)
	}
}

func TestAppendRequestQueryDropsClientAuthentication(t *testing.T) {
	incoming := url.Values{
		"alt":            {"json"},
		"page_token":     {"next-page"},
		"key":            {"client-key"},
		"API_KEY":        {"client-api-key"},
		"access_token":   {"client-access-token"},
		"Authorization":  {"Bearer client"},
		"X-Goog-Api-Key": {"client-google-key"},
	}
	endpoint, err := appendRequestQuery("https://api.example.com/v1/models", incoming, config.ProtocolGemini, false, "")
	if err != nil {
		t.Fatalf("appendRequestQuery() error = %v", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	query := parsed.Query()
	if query.Get("page_token") != "next-page" {
		t.Fatalf("ordinary query parameters were not preserved: %v", query)
	}
	for _, key := range []string{"key", "API_KEY", "access_token", "Authorization", "X-Goog-Api-Key"} {
		if _, exists := query[key]; exists {
			t.Fatalf("authentication query parameter %q was forwarded: %v", key, query)
		}
	}
}

func TestAppendRequestQueryEnforcesGeminiResponseEncoding(t *testing.T) {
	streamURL, err := appendRequestQuery(
		"https://api.example.com/v1/models/model:streamGenerateContent",
		url.Values{"alt": {"json"}},
		config.ProtocolGemini,
		true,
		"generate",
	)
	if err != nil {
		t.Fatalf("stream appendRequestQuery() error = %v", err)
	}
	parsedStream, _ := url.Parse(streamURL)
	if got := parsedStream.Query().Get("alt"); got != "sse" {
		t.Fatalf("stream alt = %q, want sse", got)
	}

	nonStreamURL, err := appendRequestQuery(
		"https://api.example.com/v1/models/model:generateContent",
		url.Values{"alt": {"sse"}, "page": {"2"}},
		config.ProtocolGemini,
		false,
		"generate",
	)
	if err != nil {
		t.Fatalf("non-stream appendRequestQuery() error = %v", err)
	}
	parsedNonStream, _ := url.Parse(nonStreamURL)
	if query := parsedNonStream.Query(); query.Has("alt") || query.Get("page") != "2" {
		t.Fatalf("non-stream query = %v", query)
	}
}

func TestExecuteStreamGeminiUsesSSEAndRewritesModel(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- capturedRequest{path: request.URL.Path, query: request.URL.Query(), headers: request.Header.Clone(), body: body}
		response.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := response.(http.Flusher)
		_, _ = response.Write([]byte("data: {\"modelVersion\":\"upstream-model\",\"candidates\":[]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1beta", config.ProtocolGemini, false)
	result, err := New(cfg).ExecuteStream(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model(8192)",
		Payload:     []byte(`{"contents":[]}`),
		Metadata:    map[string]any{requestedModelMetaKey: "team/public-model"},
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	request := <-requests
	if request.path != "/v1beta/models/upstream-model:streamGenerateContent" {
		t.Fatalf("path = %q", request.path)
	}
	if request.query.Get("alt") != "sse" {
		t.Fatalf("alt = %q, want sse", request.query.Get("alt"))
	}
	if request.headers.Get("Accept") != "text/event-stream" {
		t.Fatalf("Accept = %q", request.headers.Get("Accept"))
	}
	if gjson.GetBytes(request.body, "stream").Exists() {
		t.Fatalf("Gemini stream body contains endpoint-only stream flag: %s", request.body)
	}
	if !gjson.Valid(output.String()) {
		t.Fatalf("Gemini stream chunk is not raw JSON: %q", output.String())
	}
	if strings.Contains(output.String(), "data:") {
		t.Fatalf("Gemini stream retained upstream SSE framing: %q", output.String())
	}
	if got := gjson.Get(output.String(), "modelVersion").String(); got != "team/public-model" {
		t.Fatalf("stream model = %q, want team/public-model: %q", got, output.String())
	}
}

func TestGeminiStreamConversionStripsFragmentedSSEFraming(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 5)
	input <- pluginapi.HTTPStreamChunk{Payload: []byte(": keepalive\n")}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("event: message\ndata: {\"modelVersion\":\"upstream\",")}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("\ndata: \"candi")}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("dates\":[]}")}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("\n\ndata: [DONE]\n\n")}
	close(input)

	var output bytes.Buffer
	for chunk := range convertHTTPChunksWithClose(context.Background(), input, "team/public", true, true, nil) {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !gjson.Valid(output.String()) {
		t.Fatalf("Gemini stream output is not raw JSON: %q", output.String())
	}
	if got := gjson.Get(output.String(), "modelVersion").String(); got != "team/public" {
		t.Fatalf("modelVersion = %q, want team/public: %q", got, output.String())
	}
}

func TestGeminiStreamConversionRejectsOversizedEvent(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 1)
	input <- pluginapi.HTTPStreamChunk{Payload: append([]byte("data: {\"text\":\""), bytes.Repeat([]byte("x"), maxErrorBodyBytes)...)}
	close(input)

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 1)
	for chunk := range convertHTTPChunksWithClose(context.Background(), input, "", false, true, nil) {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
		t.Fatalf("oversized Gemini event chunks = %#v, want one error-only chunk", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "exceeds") {
		t.Fatalf("oversized Gemini event error = %q", chunks[0].Err)
	}
}

func TestGeminiStreamConversionStripsSSEWithoutForceMapping(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 1)
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"candidates\":[]}\n\n")}
	close(input)

	var output bytes.Buffer
	for chunk := range convertHTTPChunksWithClose(context.Background(), input, "", false, true, nil) {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if got := output.String(); got != `{"candidates":[]}` {
		t.Fatalf("Gemini stream output = %q, want raw JSON", got)
	}
}

func TestExecuteImageGenerationJSON(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := captureServer(requests, []byte(`{"created":1,"data":[]}`))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolAnthropic, true)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:        "image-public",
		Format:       formatOpenAIImage,
		SourceFormat: formatOpenAIImage,
		Payload:      []byte(`{"model":"image-public","prompt":"draw","stream":true}`),
		Metadata:     map[string]any{requestPathMetadataKey: "/v1/images/generations"},
		StorageJSON:  credentialJSON(cfg, "image-key"),
		HTTPClient:   &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	request := <-requests
	if request.path != "/v1/images/generations" {
		t.Fatalf("path = %q", request.path)
	}
	if request.headers.Get("Authorization") != "Bearer image-key" || request.headers.Get("X-Api-Key") != "" {
		t.Fatalf("image auth headers = %#v", request.headers)
	}
	if got := gjson.GetBytes(request.body, "model").String(); got != "upstream-image" {
		t.Fatalf("image model = %q: %s", got, request.body)
	}
	if gjson.GetBytes(request.body, "stream").Exists() {
		t.Fatalf("image non-stream body retained stream: %s", request.body)
	}
}

func TestExecuteImageEditRewritesMultipart(t *testing.T) {
	var input bytes.Buffer
	writer := multipart.NewWriter(&input)
	_ = writer.WriteField("model", "image-public")
	_ = writer.WriteField("stream", "false")
	_ = writer.WriteField("prompt", "edit this")
	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", multipart.FileContentDisposition("image", "source.webp"))
	fileHeader.Set("Content-Type", "image/webp")
	file, errFile := writer.CreatePart(fileHeader)
	if errFile != nil {
		t.Fatalf("CreatePart() error = %v", errFile)
	}
	_, _ = file.Write([]byte("webp-data"))
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("multipart Close() error = %v", errClose)
	}

	requests := make(chan capturedRequest, 1)
	server := captureServer(requests, []byte(`{"data":[]}`))
	defer server.Close()
	cfg := testConfig(server.URL+"/v1", config.ProtocolOpenAIChat, true)
	_, err := New(cfg).Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:        "image-public",
		Format:       formatOpenAIImage,
		SourceFormat: formatOpenAIImage,
		Headers:      http.Header{"Content-Type": []string{writer.FormDataContentType()}},
		Payload:      input.Bytes(),
		Metadata:     map[string]any{requestPathMetadataKey: "/v1/images/edits"},
		StorageJSON:  credentialJSON(cfg, "secret"),
		HTTPClient:   &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	request := <-requests
	if request.path != "/v1/images/edits" {
		t.Fatalf("path = %q", request.path)
	}
	mediaType, parameters, errMedia := mime.ParseMediaType(request.headers.Get("Content-Type"))
	if errMedia != nil || mediaType != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, error = %v", request.headers.Get("Content-Type"), errMedia)
	}
	form, errForm := multipart.NewReader(bytes.NewReader(request.body), parameters["boundary"]).ReadForm(1 << 20)
	if errForm != nil {
		t.Fatalf("ReadForm() error = %v", errForm)
	}
	defer form.RemoveAll()
	if got := form.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model fields = %#v", got)
	}
	if _, exists := form.Value["stream"]; exists {
		t.Fatalf("stream field was retained: %#v", form.Value["stream"])
	}
	if got := form.Value["prompt"]; len(got) != 1 || got[0] != "edit this" {
		t.Fatalf("prompt fields = %#v", got)
	}
	files := form.File["image"]
	if len(files) != 1 || files[0].Filename != "source.webp" || files[0].Header.Get("Content-Type") != "image/webp" {
		t.Fatalf("image files = %#v", files)
	}
	opened, errOpen := files[0].Open()
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	data, _ := io.ReadAll(opened)
	_ = opened.Close()
	if string(data) != "webp-data" {
		t.Fatalf("image data = %q", data)
	}
}

func TestCountTokensUsesNativeProtocolEndpoints(t *testing.T) {
	for _, test := range []struct {
		protocol config.Protocol
		path     string
	}{
		{protocol: config.ProtocolAnthropic, path: "/v1/messages/count_tokens"},
		{protocol: config.ProtocolGemini, path: "/v1/models/upstream-model:countTokens"},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			requests := make(chan capturedRequest, 1)
			server := captureServer(requests, []byte(`{"input_tokens":7,"totalTokens":7}`))
			defer server.Close()
			cfg := testConfig(server.URL+"/v1", test.protocol, false)
			result, err := New(cfg).CountTokens(context.Background(), pluginapi.ExecutorRequest{
				Model:       "public-model(high)",
				Payload:     []byte(`{"messages":[]}`),
				StorageJSON: credentialJSON(cfg, "secret"),
				HTTPClient:  &networkHostClient{},
			})
			if err != nil {
				t.Fatalf("CountTokens() error = %v", err)
			}
			request := <-requests
			if request.path != test.path {
				t.Fatalf("path = %q, want %q", request.path, test.path)
			}
			if test.protocol == config.ProtocolGemini && gjson.GetBytes(request.body, "model").Exists() {
				t.Fatalf("Gemini countTokens body contains endpoint-only model: %s", request.body)
			}
			if len(result.Payload) == 0 {
				t.Fatal("CountTokens() returned empty payload")
			}
		})
	}
}

func TestLocalCountTokensRejectsMismatchedCredentialDestination(t *testing.T) {
	cfg := testConfig("https://api.example.com/v1", config.ProtocolOpenAIResponses, false)
	other := cfg
	other.Protocol = config.ProtocolOpenAIChat
	_, err := New(cfg).CountTokens(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"input":"hello"}`),
		StorageJSON: credentialJSON(other, "private-secret"),
	})
	if err == nil {
		t.Fatal("CountTokens() accepted a credential bound to another protocol")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusServiceUnavailable || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("CountTokens() binding error = %#v", err)
	}
}

func TestUpstreamErrorsRedactSelectedAPIKeyAndConfiguredHeaders(t *testing.T) {
	const (
		apiKey       = "actual-api-key-private"
		customHeader = "configured-header-private"
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Private-Metadata") != customHeader {
			t.Errorf("X-Private-Metadata = %q, want configured value", request.Header.Get("X-Private-Metadata"))
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"error":{"type":"invalid_request_error","message":"rejected actual-api-key-private and configured-header-private"}}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolAnthropic, false)
	cfg.Headers = map[string]string{"X-Private-Metadata": customHeader}
	request := pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSON(cfg, apiKey),
		HTTPClient:  &networkHostClient{},
	}
	tests := []struct {
		name string
		call func(*Executor, pluginapi.ExecutorRequest) error
	}{
		{
			name: "Execute",
			call: func(executor *Executor, request pluginapi.ExecutorRequest) error {
				_, err := executor.Execute(context.Background(), request)
				return err
			},
		},
		{
			name: "ExecuteStream",
			call: func(executor *Executor, request pluginapi.ExecutorRequest) error {
				_, err := executor.ExecuteStream(context.Background(), request)
				return err
			},
		},
		{
			name: "CountTokens",
			call: func(executor *Executor, request pluginapi.ExecutorRequest) error {
				_, err := executor.CountTokens(context.Background(), request)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call(New(cfg), request)
			if err == nil {
				t.Fatalf("%s() error = nil", test.name)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("%s() status error = %#v", test.name, err)
			}
			scoped, ok := err.(interface{ IsRequestScoped() bool })
			if !ok || !scoped.IsRequestScoped() {
				t.Fatalf("%s() request scope = %#v", test.name, err)
			}
			message := err.Error()
			if strings.Contains(message, apiKey) || strings.Contains(message, customHeader) {
				t.Fatalf("%s() leaked a sensitive value: %q", test.name, message)
			}
			if strings.Count(message, redactedSensitiveValue) != 2 {
				t.Fatalf("%s() error = %q, want two redaction markers", test.name, message)
			}
		})
	}
}

func TestStreamErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadGateway)
		_, _ = response.Write(bytes.Repeat([]byte("x"), maxErrorBodyBytes+4096))
	}))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolOpenAIChat, false)
	_, err := New(cfg).ExecuteStream(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  &networkHostClient{},
	})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status error = %#v", err)
	}
	if len(err.Error()) > maxErrorBodyBytes {
		t.Fatalf("error length = %d, want <= %d", len(err.Error()), maxErrorBodyBytes)
	}
}

func TestStreamErrorBodyCancelsProducerAfterLimit(t *testing.T) {
	client := &cancellationHostClient{done: make(chan struct{})}
	cfg := testConfig("https://api.example.com/v1", config.ProtocolOpenAIChat, false)
	executor := New(cfg)
	executor.httpClients = func(string) (pluginapi.HostHTTPClient, error) { return client, nil }
	_, err := executor.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{
		Model:       "public-model",
		Payload:     []byte(`{"messages":[]}`),
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  client,
	})
	if err == nil {
		t.Fatal("ExecuteStream() error = nil")
	}
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("stream producer was not canceled after the error body limit")
	}
}

func TestStreamConversionStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan pluginapi.HTTPStreamChunk)
	output := convertHTTPChunks(ctx, input, "public", false)
	cancel()
	select {
	case _, ok := <-output:
		if ok {
			t.Fatal("output remained open after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("stream conversion did not stop after cancellation")
	}
}

func TestStreamConversionFramesFragmentedAndCoalescedSSEWithoutForceMapping(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 2)
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("event: message\r\ndata: {\"type\":\"content_")}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("block_delta\",\"delta\":{\"text\":\"one\"}}\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\ndata: {\"tail\":true}\n\nevent: done")}
	close(input)

	var got []string
	for chunk := range convertHTTPChunks(context.Background(), input, "", false) {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		got = append(got, string(chunk.Payload))
	}
	want := []string{
		"event: message\r\n",
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"one\"}}\r\n",
		"\r\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n",
		"\n",
		"data: {\"tail\":true}\n",
		"\n",
		"event: done",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("framed SSE chunks = %#v, want %#v", got, want)
	}
}

func TestStreamConversionRejectsOversizedSSERecord(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 2)
	input <- pluginapi.HTTPStreamChunk{Payload: append([]byte("data: "), bytes.Repeat([]byte("x"), maxErrorBodyBytes/2)...)}
	input <- pluginapi.HTTPStreamChunk{Payload: append(bytes.Repeat([]byte("x"), maxErrorBodyBytes/2+1), '\n')}
	close(input)

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 1)
	for chunk := range convertHTTPChunks(context.Background(), input, "", false) {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
		t.Fatalf("oversized SSE chunks = %#v, want one error-only chunk", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "exceeds") {
		t.Fatalf("oversized SSE error = %q", chunks[0].Err)
	}
}

func TestStreamConversionRewritesFragmentedSSELine(t *testing.T) {
	input := make(chan pluginapi.HTTPStreamChunk, 3)
	input <- pluginapi.HTTPStreamChunk{Payload: []byte(`data: {"model":"up`)}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte(`stream","delta":`)}
	input <- pluginapi.HTTPStreamChunk{Payload: []byte("{}}\n\n")}
	close(input)

	var output bytes.Buffer
	for chunk := range convertHTTPChunks(context.Background(), input, "team/public", true) {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if got := output.String(); !strings.Contains(got, `"model":"team/public"`) || strings.Contains(got, `"model":"upstream"`) {
		t.Fatalf("fragmented SSE output = %q", got)
	}
}

func TestPublicModelForRequestTracksHostPrefixRouting(t *testing.T) {
	cfg := testConfig("https://api.example.com/v1", config.ProtocolOpenAIChat, false)
	model := cfg.Models[0]
	for _, test := range []struct {
		name string
		req  pluginapi.ExecutorRequest
		want string
	}{
		{name: "unprefixed route", req: pluginapi.ExecutorRequest{Model: "public-model"}, want: "public-model"},
		{name: "prefixed executor model", req: pluginapi.ExecutorRequest{Model: "team/public-model(high)"}, want: "team/public-model"},
		{name: "host stripped prefix", req: pluginapi.ExecutorRequest{Model: "public-model(high)", Metadata: map[string]any{requestedModelMetaKey: "team/public-model(high)"}}, want: "team/public-model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := publicModelForRequest(cfg, test.req, model); got != test.want {
				t.Fatalf("publicModelForRequest() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCredentialPoolStorageFailsClosedUntilHostReconciliation(t *testing.T) {
	_, err := apiKeyFromStorage([]byte(`{"type":"multi-protocol-provider","keys":[{"api-key":"must-not-be-selected-here"}]}`))
	if err == nil {
		t.Fatal("apiKeyFromStorage() accepted an unreconciled credential pool")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("apiKeyFromStorage() error = %#v", err)
	}
	if strings.Contains(err.Error(), "must-not-be-selected-here") {
		t.Fatalf("apiKeyFromStorage() leaked a pool secret: %v", err)
	}
}

func TestCredentialDestinationBindingFailsClosed(t *testing.T) {
	cfg := testConfig("https://current.example.com/v1", config.ProtocolOpenAIChat, false)
	other := testConfig("https://other.example.com/v1", config.ProtocolOpenAIChat, false)
	otherProtocol := cfg
	otherProtocol.Protocol = config.ProtocolAnthropic
	for _, test := range []struct {
		name    string
		storage []byte
	}{
		{name: "unbound", storage: []byte(`{"type":"multi-protocol-provider","api-key":"private-secret"}`)},
		{name: "different base URL", storage: credentialJSON(other, "private-secret")},
		{name: "different protocol", storage: credentialJSON(otherProtocol, "private-secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := apiKeyFromStorageForConfig(test.storage, cfg)
			if err == nil {
				t.Fatal("apiKeyFromStorageForConfig() error = nil")
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("binding error = %#v", err)
			}
			if strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("binding error leaked API key: %v", err)
			}
		})
	}
}

func TestHttpRequestRestrictsOriginAndUsesProviderAuth(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := captureServer(requests, []byte(`{"ok":true}`))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolAnthropic, false)
	executor := New(cfg)
	_, err := executor.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodGet,
		URL:         server.URL + "/v1/models?page_token=next&key=caller-key&api_key=caller-api-key",
		Headers:     http.Header{"Authorization": []string{"Bearer caller"}},
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("HttpRequest() error = %v", err)
	}
	request := <-requests
	if request.headers.Get("Authorization") != "" || request.headers.Get("X-Api-Key") != "secret" {
		t.Fatalf("auth headers = %#v", request.headers)
	}
	if request.query.Get("page_token") != "next" || request.query.Has("key") || request.query.Has("api_key") {
		t.Fatalf("sanitized HTTP request query = %v", request.query)
	}
	_, err = executor.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
		URL:         "https://example.invalid/v1/models",
		StorageJSON: credentialJSON(cfg, "secret"),
		HTTPClient:  &networkHostClient{},
	})
	if err == nil {
		t.Fatal("HttpRequest() accepted a different origin")
	}
}

func TestHttpRequestRedactsNonSuccessResponseBody(t *testing.T) {
	const (
		apiKey       = "http-api-key-private"
		customHeader = "http-header-private"
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Api-Key") != apiKey || request.Header.Get("X-Private-Metadata") != customHeader {
			t.Errorf("upstream request headers = %#v", request.Header)
		}
		response.Header().Set("X-Upstream", "preserved")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"message":"echo http-api-key-private and http-header-private"}`))
	}))
	defer server.Close()

	cfg := testConfig(server.URL+"/v1", config.ProtocolAnthropic, false)
	cfg.Headers = map[string]string{"X-Private-Metadata": customHeader}
	response, err := New(cfg).HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodGet,
		URL:         server.URL + "/v1/models",
		StorageJSON: credentialJSON(cfg, apiKey),
		HTTPClient:  &networkHostClient{},
	})
	if err != nil {
		t.Fatalf("HttpRequest() error = %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized || response.Headers.Get("X-Upstream") != "preserved" {
		t.Fatalf("HttpRequest() response = status %d, headers %#v", response.StatusCode, response.Headers)
	}
	body := string(response.Body)
	if strings.Contains(body, apiKey) || strings.Contains(body, customHeader) {
		t.Fatalf("HttpRequest() body leaked a sensitive value: %q", body)
	}
	if strings.Count(body, redactedSensitiveValue) != 2 {
		t.Fatalf("HttpRequest() body = %q, want two redaction markers", body)
	}
}

func testConfig(baseURL string, protocol config.Protocol, image bool) config.Config {
	model := config.Model{
		Name:         "upstream-model",
		Alias:        "public-model",
		ForceMapping: true,
	}
	if image {
		model = config.Model{Name: "upstream-image", Alias: "image-public", Image: true}
	}
	return config.Config{
		Name:     "Test Provider",
		Protocol: protocol,
		BaseURL:  baseURL,
		Prefix:   "team",
		Headers:  map[string]string{"X-Custom": "configured"},
		Models:   []config.Model{model},
	}
}

func credentialJSON(cfg config.Config, key string) []byte {
	raw, err := json.Marshal(map[string]any{
		"type":        "multi-protocol-provider",
		"destination": providerauth.DestinationForConfig(cfg),
		"api-key":     key,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func captureServer(requests chan<- capturedRequest, responseBody []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- capturedRequest{
			path:    request.URL.Path,
			query:   request.URL.Query(),
			headers: request.Header.Clone(),
			body:    body,
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(responseBody)
	}))
}

type networkHostClient struct {
	client *http.Client
}

type cancellationHostClient struct {
	done chan struct{}
}

func (c *cancellationHostClient) Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{}, errors.New("unexpected non-stream request")
}

func (c *cancellationHostClient) DoStream(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		chunks <- pluginapi.HTTPStreamChunk{Payload: bytes.Repeat([]byte("x"), maxErrorBodyBytes+1)}
		select {
		case chunks <- pluginapi.HTTPStreamChunk{Payload: []byte("unreachable")}:
		case <-ctx.Done():
			close(c.done)
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: http.StatusBadGateway, Chunks: chunks}, nil
}

func (c *networkHostClient) httpClient() *http.Client {
	if c.client != nil {
		return c.client
	}
	return http.DefaultClient
}

func (c *networkHostClient) Do(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	httpRequest, errRequest := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if errRequest != nil {
		return pluginapi.HTTPResponse{}, errRequest
	}
	httpRequest.Header = request.Headers.Clone()
	response, errDo := c.httpClient().Do(httpRequest)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer response.Body.Close()
	body, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, errRead
	}
	return pluginapi.HTTPResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Body: body}, nil
}

func (c *networkHostClient) DoStream(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	httpRequest, errRequest := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if errRequest != nil {
		return pluginapi.HTTPStreamResponse{}, errRequest
	}
	httpRequest.Header = request.Headers.Clone()
	response, errDo := c.httpClient().Do(httpRequest)
	if errDo != nil {
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer response.Body.Close()
		buffer := make([]byte, 4096)
		for {
			count, errRead := response.Body.Read(buffer)
			if count > 0 {
				chunk := pluginapi.HTTPStreamChunk{Payload: append([]byte(nil), buffer[:count]...)}
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					select {
					case chunks <- pluginapi.HTTPStreamChunk{Err: errRead}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Chunks: chunks}, nil
}
