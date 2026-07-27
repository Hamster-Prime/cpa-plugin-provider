package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	formatOpenAIImage      = "openai-image"
	requestPathMetadataKey = "request_path"
	requestedModelMetaKey  = "requested_model"
)

type builtRequest struct {
	method          string
	url             string
	headers         http.Header
	body            []byte
	model           config.Model
	publicModel     string
	forceMapping    bool
	sensitiveValues []string
}

type authStorage struct {
	Type        string                              `json:"type"`
	Destination *providerauth.CredentialDestination `json:"destination"`
	APIKey      string                              `json:"api-key"`
	APIKeyAlt   string                              `json:"api_key"`
	Key         string                              `json:"key"`
	Token       string                              `json:"token"`
	AccessToken string                              `json:"access_token"`
	Keys        json.RawMessage                     `json:"keys"`
}

func buildModelRequest(cfg config.Config, req pluginapi.ExecutorRequest, stream bool, action string) (builtRequest, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return builtRequest{}, statusError{statusCode: http.StatusServiceUnavailable, body: []byte("provider base URL is not configured")}
	}
	model, found := cfg.ResolveModel(req.Model)
	if !found {
		return builtRequest{}, newRequestScopedStatusError(http.StatusBadRequest, []byte(fmt.Sprintf("model %q is not configured", req.Model)))
	}
	apiKey, errKey := apiKeyFromStorageForConfig(req.StorageJSON, cfg)
	if errKey != nil {
		return builtRequest{}, errKey
	}

	endpoint, image := requestEndpoint(cfg, req, model, stream, action)
	if endpoint == "" {
		return builtRequest{}, newRequestScopedStatusError(http.StatusBadRequest, []byte("unsupported provider request"))
	}
	body := append([]byte(nil), req.Payload...)
	contentType := strings.TrimSpace(req.Headers.Get("Content-Type"))
	if image {
		var errPrepare error
		body, contentType, errPrepare = prepareImagePayload(body, model.Name, contentType, stream)
		if errPrepare != nil {
			return builtRequest{}, newRequestScopedStatusError(http.StatusBadRequest, []byte(errPrepare.Error()))
		}
	} else {
		var errPrepare error
		body, errPrepare = prepareJSONPayload(body, model.Name, stream, cfg.Protocol, req.Alt, action)
		if errPrepare != nil {
			return builtRequest{}, newRequestScopedStatusError(http.StatusBadRequest, []byte(errPrepare.Error()))
		}
		contentType = "application/json"
	}

	headers := requestHeaders(cfg, req.Headers, apiKey, image, stream, contentType)
	endpoint, errQuery := appendRequestQuery(endpoint, req.Query, cfg.Protocol, stream, action)
	if errQuery != nil {
		return builtRequest{}, errQuery
	}
	return builtRequest{
		method:          http.MethodPost,
		url:             endpoint,
		headers:         headers,
		body:            body,
		model:           model,
		publicModel:     publicModelForRequest(cfg, req, model),
		forceMapping:    model.ForceMapping,
		sensitiveValues: upstreamSensitiveValues(apiKey, cfg.Headers),
	}, nil
}

func upstreamSensitiveValues(apiKey string, headers map[string]string) []string {
	values := make([]string, 0, len(headers)+1)
	if apiKey != "" {
		values = append(values, apiKey)
	}
	for _, value := range headers {
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func publicModelForRequest(cfg config.Config, req pluginapi.ExecutorRequest, model config.Model) string {
	requested := strings.TrimSpace(req.Model)
	if value, ok := req.Metadata[requestedModelMetaKey].(string); ok && strings.TrimSpace(value) != "" {
		requested = strings.TrimSpace(value)
	}
	if cfg.Prefix != "" && strings.HasPrefix(requested, cfg.Prefix+"/") {
		return cfg.ModelID(model)
	}
	return cfg.BaseModelID(model)
}

func requestEndpoint(cfg config.Config, req pluginapi.ExecutorRequest, model config.Model, stream bool, action string) (string, bool) {
	if isImageRequest(req) {
		path := "/images/generations"
		if strings.HasSuffix(requestPath(req.Metadata), "/images/edits") {
			path = "/images/edits"
		}
		return joinEndpoint(cfg.BaseURL, path), true
	}

	path := ""
	switch cfg.Protocol {
	case config.ProtocolOpenAIChat:
		path = "/chat/completions"
	case config.ProtocolOpenAIResponses:
		path = "/responses"
		if req.Alt == "responses/compact" {
			path = "/responses/compact"
		}
	case config.ProtocolAnthropic:
		path = "/messages"
		if action == "countTokens" {
			path = "/messages/count_tokens"
		}
	case config.ProtocolGemini:
		operation := "generateContent"
		if action == "countTokens" {
			operation = "countTokens"
		} else if stream {
			operation = "streamGenerateContent"
		}
		modelName := strings.TrimPrefix(strings.TrimSpace(model.Name), "models/")
		path = "/models/" + escapeModelPath(modelName) + ":" + operation
	}
	if path == "" {
		return "", false
	}
	return joinEndpoint(cfg.BaseURL, path), false
}

func escapeModelPath(model string) string {
	parts := strings.Split(strings.Trim(model, "/"), "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func joinEndpoint(baseURL, endpointPath string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/" + strings.TrimLeft(endpointPath, "/")
}

func appendRequestQuery(endpoint string, incoming url.Values, protocol config.Protocol, stream bool, action string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse provider endpoint: %w", err)
	}
	query := parsed.Query()
	for key, values := range incoming {
		if isAuthenticationQueryParameter(key) {
			continue
		}
		for _, value := range values {
			query.Add(key, value)
		}
	}
	if protocol == config.ProtocolGemini {
		if stream && action != "countTokens" {
			query.Set("alt", "sse")
		} else {
			query.Del("alt")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func isAuthenticationQueryParameter(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "-")
	switch key {
	case "key", "api-key", "apikey", "access-token", "accesstoken", "token", "authorization", "x-api-key", "x-goog-api-key":
		return true
	default:
		return false
	}
}

func stripAuthenticationQuery(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", newRequestScopedStatusError(http.StatusBadRequest, []byte("request URL must be valid"))
	}
	query := parsed.Query()
	for key := range query {
		if isAuthenticationQueryParameter(key) {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func prepareJSONPayload(payload []byte, model string, stream bool, protocol config.Protocol, alt, action string) ([]byte, error) {
	if !json.Valid(payload) {
		return nil, fmt.Errorf("request body must be valid JSON")
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	updated := append([]byte(nil), payload...)
	var err error
	if protocol == config.ProtocolGemini {
		updated, err = sjson.DeleteBytes(updated, "model")
	} else {
		updated, err = sjson.SetBytes(updated, "model", model)
	}
	if err != nil {
		return nil, fmt.Errorf("normalize upstream model: %w", err)
	}
	if action == "countTokens" {
		updated, _ = sjson.DeleteBytes(updated, "stream")
		return updated, nil
	}
	if protocol == config.ProtocolOpenAIResponses && alt == "responses/compact" {
		updated, _ = sjson.DeleteBytes(updated, "stream")
		return updated, nil
	}
	if protocol == config.ProtocolGemini {
		updated, err = sjson.DeleteBytes(updated, "stream")
		if err != nil {
			return nil, fmt.Errorf("normalize Gemini stream mode: %w", err)
		}
		return updated, nil
	}
	if stream {
		updated, err = sjson.SetBytes(updated, "stream", true)
	} else {
		updated, err = sjson.DeleteBytes(updated, "stream")
	}
	if err != nil {
		return nil, fmt.Errorf("set stream mode: %w", err)
	}
	return updated, nil
}

func prepareImagePayload(payload []byte, model, contentType string, stream bool) ([]byte, string, error) {
	if json.Valid(payload) {
		updated, err := sjson.SetBytes(payload, "model", model)
		if err != nil {
			return nil, "", fmt.Errorf("set image model: %w", err)
		}
		if stream {
			updated, err = sjson.SetBytes(updated, "stream", true)
		} else {
			updated, err = sjson.DeleteBytes(updated, "stream")
		}
		if err != nil {
			return nil, "", fmt.Errorf("set image stream mode: %w", err)
		}
		return updated, "application/json", nil
	}

	mediaType, params, errMedia := mime.ParseMediaType(contentType)
	if errMedia != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return nil, "", fmt.Errorf("image request must be JSON or multipart form data")
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteImageMultipart(payload, boundary, model, stream)
}

func rewriteImageMultipart(payload []byte, boundary, model string, stream bool) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", fmt.Errorf("write image model field: %w", err)
	}
	if stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			return nil, "", fmt.Errorf("write image stream field: %w", err)
		}
	}
	for {
		part, errPart := reader.NextPart()
		if errPart == io.EOF {
			break
		}
		if errPart != nil {
			return nil, "", fmt.Errorf("read multipart image body: %w", errPart)
		}
		if part.FormName() == "model" || part.FormName() == "stream" {
			if errClose := part.Close(); errClose != nil {
				return nil, "", fmt.Errorf("close replaced multipart field: %w", errClose)
			}
			continue
		}
		header := cloneMIMEHeader(part.Header)
		destination, errCreate := writer.CreatePart(header)
		if errCreate != nil {
			_ = part.Close()
			return nil, "", fmt.Errorf("create multipart image field: %w", errCreate)
		}
		_, errCopy := io.Copy(destination, part)
		errClose := part.Close()
		if errCopy != nil {
			return nil, "", fmt.Errorf("copy multipart image field: %w", errCopy)
		}
		if errClose != nil {
			return nil, "", fmt.Errorf("close multipart image field: %w", errClose)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish multipart image body: %w", err)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func cloneMIMEHeader(source textproto.MIMEHeader) textproto.MIMEHeader {
	cloned := make(textproto.MIMEHeader, len(source))
	for key, values := range source {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func requestHeaders(cfg config.Config, incoming http.Header, apiKey string, image, stream bool, contentType string) http.Header {
	headers := make(http.Header)
	copyHeader(headers, incoming, "OpenAI-Organization")
	copyHeader(headers, incoming, "OpenAI-Project")
	copyHeader(headers, incoming, "Anthropic-Beta")
	copyHeader(headers, incoming, "Anthropic-Version")
	if contentType == "" {
		contentType = "application/json"
	}
	headers.Set("Content-Type", contentType)
	headers.Set("User-Agent", "cpa-plugin-"+provider.ID)
	if stream {
		headers.Set("Accept", "text/event-stream")
		headers.Set("Cache-Control", "no-cache")
	} else {
		headers.Set("Accept", "application/json")
	}
	applyAuthentication(headers, cfg.Protocol, apiKey, image)
	for key, value := range cfg.Headers {
		if strings.TrimSpace(key) != "" {
			headers.Set(key, value)
		}
	}
	return headers
}

func applyAuthentication(headers http.Header, protocol config.Protocol, apiKey string, image bool) {
	headers.Del("Authorization")
	headers.Del("X-Api-Key")
	headers.Del("X-Goog-Api-Key")
	if image || protocol == config.ProtocolOpenAIChat || protocol == config.ProtocolOpenAIResponses {
		headers.Set("Authorization", "Bearer "+apiKey)
		return
	}
	if protocol == config.ProtocolAnthropic {
		headers.Set("X-Api-Key", apiKey)
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
		return
	}
	headers.Set("X-Goog-Api-Key", apiKey)
}

func copyHeader(destination, source http.Header, name string) {
	for _, value := range source.Values(name) {
		destination.Add(name, value)
	}
}

func apiKeyFromStorage(raw []byte) (string, error) {
	key, _, err := credentialFromStorage(raw)
	return key, err
}

func apiKeyFromStorageForConfig(raw []byte, cfg config.Config) (string, error) {
	key, destination, err := credentialFromStorage(raw)
	if err != nil {
		return "", err
	}
	if destination == nil {
		return "", statusError{statusCode: http.StatusServiceUnavailable, body: []byte("provider credential is not bound to an upstream destination")}
	}
	if !destination.MatchesConfig(cfg) {
		return "", statusError{statusCode: http.StatusServiceUnavailable, body: []byte("provider credential is bound to a different upstream destination")}
	}
	return key, nil
}

func credentialFromStorage(raw []byte) (string, *providerauth.CredentialDestination, error) {
	var storage authStorage
	if err := json.Unmarshal(raw, &storage); err != nil {
		return "", nil, statusError{statusCode: http.StatusUnauthorized, body: []byte("invalid provider credential")}
	}
	if storage.Type != "" && storage.Type != provider.ID {
		return "", nil, statusError{statusCode: http.StatusUnauthorized, body: []byte("credential belongs to another provider")}
	}
	if len(storage.Keys) > 0 {
		return "", nil, statusError{statusCode: http.StatusServiceUnavailable, body: []byte("provider credential pool is awaiting CPA auth reconciliation")}
	}
	for _, candidate := range []string{storage.APIKey, storage.APIKeyAlt, storage.Key, storage.Token, storage.AccessToken} {
		if key := strings.TrimSpace(candidate); key != "" {
			return key, storage.Destination, nil
		}
	}
	return "", nil, statusError{statusCode: http.StatusUnauthorized, body: []byte("provider API key is missing")}
}

func isImageRequest(req pluginapi.ExecutorRequest) bool {
	return strings.EqualFold(strings.TrimSpace(req.SourceFormat), formatOpenAIImage) ||
		strings.EqualFold(strings.TrimSpace(req.Format), formatOpenAIImage)
}

func requestPath(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	switch value := metadata[requestPathMetadataKey].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func validateExecutorURL(baseURL, target string) error {
	base, errBase := url.Parse(strings.TrimSpace(baseURL))
	targetURL, errTarget := url.Parse(strings.TrimSpace(target))
	if errBase != nil || errTarget != nil || base.Scheme == "" || base.Host == "" || targetURL.Scheme == "" || targetURL.Host == "" {
		return newRequestScopedStatusError(http.StatusBadRequest, []byte("request URL must be absolute"))
	}
	if !strings.EqualFold(base.Scheme, targetURL.Scheme) || !strings.EqualFold(base.Host, targetURL.Host) {
		return newRequestScopedStatusError(http.StatusBadRequest, []byte("request URL must use the configured provider origin"))
	}
	return nil
}
