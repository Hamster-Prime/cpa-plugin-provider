package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/executor"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/ui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	maxRequestBody       = 2 << 20
	maxModelResponseBody = 2 << 20
	maxKeys              = 128
	defaultAuthSyncWait  = 10 * time.Second
	defaultAuthSyncPoll  = 100 * time.Millisecond
)

// HostAuthStore is the narrow host callback surface needed by the management
// API. Dynamic ABI bridges can implement it with host.auth.list/get/save.
type HostAuthStore interface {
	ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error)
	GetAuth(context.Context, pluginapi.HostAuthGetRequest) (pluginapi.HostAuthGetResponse, error)
	SaveAuth(context.Context, pluginapi.HostAuthSaveRequest) (pluginapi.HostAuthSaveResponse, error)
}

// ConnectionTester executes one non-streaming probe with the submitted draft
// configuration. TestRequest.Key always contains a resolved, non-empty secret.
type ConnectionTester interface {
	TestConnection(context.Context, TestRequest) (TestResult, error)
}

type TestRequest struct {
	Config     config.Config
	Key        providerauth.Key
	Model      string
	HTTPClient pluginapi.HostHTTPClient
}

type TestResult struct {
	Message    string `json:"message"`
	StatusCode int    `json:"status-code,omitempty"`
	LatencyMS  int64  `json:"latency-ms,omitempty"`
	Model      string `json:"model,omitempty"`
}

type httpClientContextKey struct{}

// WithHTTPClient attaches the callback-scoped host transport used by a
// Management API connection probe.
func WithHTTPClient(ctx context.Context, client pluginapi.HostHTTPClient) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpClientContextKey{}, client)
}

func httpClientFromContext(ctx context.Context) pluginapi.HostHTTPClient {
	if ctx == nil {
		return nil
	}
	client, _ := ctx.Value(httpClientContextKey{}).(pluginapi.HostHTTPClient)
	return client
}

type API struct {
	pluginID       string
	credentialFile string
	config         config.Config
	authStore      HostAuthStore
	tester         ConnectionTester
	httpClients    func(string) (pluginapi.HostHTTPClient, error)
	authSyncWait   time.Duration
	authSyncPoll   time.Duration
	credentialMu   sync.Mutex
}

func New(pluginID, credentialFile string, cfg config.Config, authStore HostAuthStore, tester ConnectionTester) *API {
	pluginID = strings.Trim(strings.TrimSpace(pluginID), "/")
	if pluginID == "" {
		pluginID = provider.ID
	}
	credentialFile = strings.TrimSpace(credentialFile)
	if credentialFile == "" {
		credentialFile = provider.CredentialsFile
	}
	cfg.Normalize()
	return &API{
		pluginID:       pluginID,
		credentialFile: credentialFile,
		config:         cfg,
		authStore:      authStore,
		tester:         tester,
		httpClients: func(proxyURL string) (pluginapi.HostHTTPClient, error) {
			return executor.NewHTTPClientWithResponseLimit(proxyURL, maxModelResponseBody)
		},
		authSyncWait: defaultAuthSyncWait,
		authSyncPoll: defaultAuthSyncPoll,
	}
}

func (a *API) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	if a == nil {
		return pluginapi.ManagementRegistrationResponse{}, errors.New("management API is unavailable")
	}
	base := "/plugins/" + a.pluginID
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: base + "/state", Description: "Returns provider configuration and masked credentials.", Handler: a},
			{Method: http.MethodPost, Path: base + "/validate", Description: "Validates and normalizes a draft provider configuration.", Handler: a},
			{Method: http.MethodPut, Path: base + "/keys", Description: "Persists the provider credential pool.", Handler: a},
			{Method: http.MethodPost, Path: base + "/models", Description: "Discovers models from a draft provider endpoint.", Handler: a},
			{Method: http.MethodPost, Path: base + "/test", Description: "Tests the provider connection.", Handler: a},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/provider",
			Menu:        "通用提供商",
			Description: "配置多协议自定义提供商",
			Handler:     a,
		}},
	}, nil
}

func (a *API) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if a == nil {
		return jsonError(http.StatusServiceUnavailable, "management_unavailable", "management API is unavailable"), nil
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/provider"):
		return resourceResponse(), nil
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/state"):
		return a.state(ctx), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/validate"):
		return a.validateConfig(req.Body), nil
	case req.Method == http.MethodPut && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/keys"):
		return a.saveKeys(ctx, req.Body), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/models"):
		return a.discoverModels(ctx, req.Body), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/test"):
		return a.testConnection(ctx, req.Body), nil
	default:
		return jsonError(http.StatusNotFound, "route_not_found", "management route not found"), nil
	}
}

type keyView struct {
	ID                 string `json:"id"`
	Label              string `json:"label,omitempty"`
	Masked             string `json:"masked,omitempty"`
	SecretPresent      bool   `json:"secret-present"`
	ProxyURL           string `json:"proxy-url,omitempty"`
	ProxySecretPresent bool   `json:"proxy-secret-present,omitempty"`
	Priority           int    `json:"priority,omitempty"`
	Disabled           bool   `json:"disabled,omitempty"`
}

func (a *API) state(ctx context.Context) pluginapi.ManagementResponse {
	credential, err := a.loadCredential(ctx)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "credential_read_failed", "failed to read provider credentials")
	}
	keys := make([]keyView, 0, len(credential.Keys))
	for _, key := range credential.Keys {
		keys = append(keys, viewKey(key))
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"config":                 a.config,
		"credential-destination": credential.Destination,
		"keys":                   keys,
	})
}

type validateConfigRequest struct {
	Config config.Config `json:"config"`
}

func (a *API) validateConfig(body []byte) pluginapi.ManagementResponse {
	var input validateConfigRequest
	if err := decodeJSON(body, &input); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_body", err.Error())
	}
	input.Config.Normalize()
	if err := input.Config.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_config", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"status": "ok",
		"config": input.Config,
	})
}

type saveKeysRequest struct {
	Keys        []editableKey                      `json:"keys"`
	Destination providerauth.CredentialDestination `json:"destination"`
}

type editableKey struct {
	ID               string `json:"id,omitempty"`
	Label            string `json:"label,omitempty"`
	APIKey           string `json:"api-key,omitempty"`
	ProxyURL         string `json:"proxy-url,omitempty"`
	PreserveProxyURL bool   `json:"preserve-proxy-url,omitempty"`
	Priority         int    `json:"priority,omitempty"`
	Disabled         bool   `json:"disabled,omitempty"`
}

func (k editableKey) authKey() providerauth.Key {
	return providerauth.Key{
		ID: k.ID, Label: k.Label, APIKey: k.APIKey, ProxyURL: k.ProxyURL,
		Priority: k.Priority, Disabled: k.Disabled,
	}
}

func (a *API) saveKeys(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	if a.authStore == nil {
		return jsonError(http.StatusServiceUnavailable, "auth_store_unavailable", "credential storage is unavailable")
	}
	if !a.config.Disabled {
		return jsonError(http.StatusConflict, "provider_must_be_disabled", "disable the provider before changing credentials")
	}
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	var input saveKeysRequest
	if err := decodeJSON(body, &input); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_body", err.Error())
	}
	input.Destination.Normalize()
	if err := input.Destination.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_destination", err.Error())
	}
	if len(input.Keys) > maxKeys {
		return jsonError(http.StatusBadRequest, "too_many_keys", fmt.Sprintf("at most %d API keys are allowed", maxKeys))
	}
	existing, err := a.loadCredential(ctx)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "credential_read_failed", "failed to read existing provider credentials")
	}
	existingKeys := make(map[string]providerauth.Key, len(existing.Keys))
	for _, key := range existing.Keys {
		existingKeys[key.ID] = key
	}

	destinationChanged := !input.Destination.MatchesConfig(a.config) ||
		existing.Destination == nil || !existing.Destination.Equal(input.Destination)
	keys := make([]providerauth.Key, 0, len(input.Keys))
	for index, draft := range input.Keys {
		key := draft.authKey()
		key.ID = strings.TrimSpace(key.ID)
		if key.ID == "" {
			key.ID = randomKeyID()
		}
		stored, exists := existingKeys[key.ID]
		if destinationChanged {
			displayProxyURL, proxySecretPresent := providerauth.DisplayProxyURL(stored.ProxyURL)
			reusesRedactedProxy := exists && proxySecretPresent && strings.TrimSpace(key.ProxyURL) == displayProxyURL
			if strings.TrimSpace(key.APIKey) == "" || draft.PreserveProxyURL || reusesRedactedProxy {
				return jsonError(http.StatusBadRequest, "credential_reentry_required", fmt.Sprintf("keys[%d] must re-enter its API key and authenticated proxy URL after changing the provider destination", index))
			}
		}
		if draft.PreserveProxyURL {
			if !exists {
				return jsonError(http.StatusBadRequest, "invalid_key", fmt.Sprintf("keys[%d].preserve-proxy-url requires an existing key", index))
			}
			key.ProxyURL = stored.ProxyURL
		}
		if err := validateEditableKey(key, index); err != nil {
			return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
		}
		if strings.TrimSpace(key.APIKey) == "" {
			key.APIKey = stored.APIKey
		}
		if strings.TrimSpace(key.APIKey) == "" {
			return jsonError(http.StatusBadRequest, "missing_secret", fmt.Sprintf("keys[%d].api-key is required for a new key", index))
		}
		keys = append(keys, key)
	}
	destination := input.Destination
	credential := providerauth.CredentialFile{Type: a.pluginID, Destination: &destination, Keys: keys}
	raw, err := providerauth.MarshalCredentialFile(credential)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_credentials", err.Error())
	}
	existingRaw, errExisting := providerauth.MarshalCredentialFile(existing)
	savedAt := time.Time{}
	if errExisting != nil || !bytes.Equal(existingRaw, raw) {
		savedAt = time.Now().UTC()
		if _, err = a.authStore.SaveAuth(ctx, pluginapi.HostAuthSaveRequest{
			Name: a.credentialFile,
			JSON: json.RawMessage(raw),
		}); err != nil {
			return jsonError(http.StatusInternalServerError, "credential_save_failed", "failed to save provider credentials")
		}
	}
	if err = a.waitForAuthConvergence(ctx, keys, savedAt); err != nil {
		return jsonError(
			http.StatusGatewayTimeout,
			"auth_reconcile_timeout",
			"credential file was saved, but CPA did not finish reconciling the controller and key records before the timeout; the provider remains disabled",
		)
	}
	return jsonResponse(http.StatusOK, map[string]any{"status": "ok", "keys": keyViews(keys)})
}

func (a *API) waitForAuthConvergence(ctx context.Context, keys []providerauth.Key, updatedAfter time.Time) error {
	wait := a.authSyncWait
	if wait <= 0 {
		wait = defaultAuthSyncWait
	}
	poll := a.authSyncPoll
	if poll <= 0 {
		poll = defaultAuthSyncPoll
	}
	expected := make(map[string]bool, len(keys))
	for _, key := range keys {
		expected[a.credentialFile+"#"+key.ID] = key.Disabled
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var lastErr error
	for {
		entries, err := a.authStore.ListAuth(ctx)
		if err == nil {
			if authEntriesConverged(entries, a.credentialFile, expected, updatedAfter) {
				return nil
			}
		} else {
			lastErr = err
		}

		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-deadline.C:
			timer.Stop()
			if lastErr != nil {
				return fmt.Errorf("auth reconciliation timed out after host list error: %w", lastErr)
			}
			return errors.New("auth reconciliation timed out")
		case <-timer.C:
		}
	}
}

func authEntriesConverged(entries []pluginapi.HostAuthFileEntry, credentialFile string, expected map[string]bool, updatedAfter time.Time) bool {
	controllerReady := false
	found := make(map[string]struct{}, len(expected))
	prefix := credentialFile + "#"
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == credentialFile {
			controllerReady = entry.Disabled && authEntryFreshEnough(entry, updatedAfter)
			continue
		}
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		disabled, ok := expected[id]
		if !ok {
			return false
		}
		if entry.Disabled != disabled || !authEntryFreshEnough(entry, updatedAfter) {
			continue
		}
		found[id] = struct{}{}
	}
	return controllerReady && len(found) == len(expected)
}

func authEntryFreshEnough(entry pluginapi.HostAuthFileEntry, updatedAfter time.Time) bool {
	if updatedAfter.IsZero() {
		return true
	}
	return !entry.UpdatedAt.IsZero() && !entry.UpdatedAt.Before(updatedAfter)
}

type discoverModelsInput struct {
	Config config.Config `json:"config"`
	Key    editableKey   `json:"key"`
}

type discoveredModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"display-name,omitempty"`
}

func (a *API) discoverModels(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var input discoverModelsInput
	if err := decodeJSON(body, &input); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_body", err.Error())
	}
	input.Config.Normalize()
	if err := input.Config.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_config", err.Error())
	}
	if input.Config.BaseURL == "" {
		return jsonError(http.StatusBadRequest, "base_url_required", "base-url is required for model discovery")
	}
	key, failure, ok := a.resolveDraftKey(ctx, input.Config, input.Key, "model discovery")
	if !ok {
		return failure
	}
	clientFactory := a.httpClients
	if clientFactory == nil {
		clientFactory = executor.NewHTTPClient
	}
	client, errClient := clientFactory(key.ProxyURL)
	if errClient != nil {
		return jsonError(http.StatusBadRequest, "invalid_proxy", "failed to create the configured proxy transport")
	}

	headers := http.Header{
		"Accept":     []string{"application/json"},
		"User-Agent": []string{"cpa-plugin-" + provider.ID},
	}
	applyDiscoveryAuthentication(headers, input.Config.Protocol, key.APIKey)
	for name, value := range input.Config.Headers {
		if strings.TrimSpace(name) != "" {
			headers.Set(name, value)
		}
	}
	response, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     input.Config.BaseURL + "/models",
		Headers: headers,
	})
	if err != nil {
		if errors.Is(err, executor.ErrResponseBodyTooLarge) {
			return jsonError(http.StatusBadGateway, "model_response_too_large", "model discovery response is too large")
		}
		message := redactDiscoveryError(err.Error(), key, input.Config.Headers)
		if strings.TrimSpace(message) == "" {
			message = "model discovery request failed"
		}
		return jsonError(http.StatusBadGateway, "model_discovery_failed", message)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return jsonError(
			http.StatusBadGateway,
			"model_discovery_failed",
			fmt.Sprintf("model discovery returned HTTP %d", response.StatusCode),
		)
	}
	if len(response.Body) > maxModelResponseBody {
		return jsonError(http.StatusBadGateway, "model_response_too_large", "model discovery response is too large")
	}
	models, err := parseDiscoveredModels(input.Config.Protocol, response.Body)
	if err != nil {
		return jsonError(http.StatusBadGateway, "invalid_model_response", "model discovery returned an unsupported response")
	}
	return jsonResponse(http.StatusOK, map[string]any{"models": models})
}

func applyDiscoveryAuthentication(headers http.Header, protocol config.Protocol, apiKey string) {
	if protocol == config.ProtocolAnthropic {
		headers.Set("X-Api-Key", apiKey)
		headers.Set("Anthropic-Version", "2023-06-01")
		return
	}
	if protocol == config.ProtocolGemini {
		headers.Set("X-Goog-Api-Key", apiKey)
		return
	}
	headers.Set("Authorization", "Bearer "+apiKey)
}

func parseDiscoveredModels(protocol config.Protocol, body []byte) ([]discoveredModel, error) {
	models := make([]discoveredModel, 0)
	seen := make(map[string]struct{})
	appendModel := func(name, displayName string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		models = append(models, discoveredModel{Name: name, DisplayName: strings.TrimSpace(displayName)})
	}

	if protocol == config.ProtocolGemini {
		var payload struct {
			Models []struct {
				Name                       string   `json:"name"`
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Models == nil {
			return nil, errors.New("invalid Gemini models response")
		}
		for _, model := range payload.Models {
			if len(model.SupportedGenerationMethods) > 0 && !containsFold(model.SupportedGenerationMethods, "generateContent") {
				continue
			}
			appendModel(strings.TrimPrefix(strings.TrimSpace(model.Name), "models/"), model.DisplayName)
		}
		return models, nil
	}

	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, errors.New("invalid OpenAI-compatible models response")
	}
	for _, model := range payload.Data {
		displayName := model.DisplayName
		if displayName == "" {
			displayName = model.Name
		}
		appendModel(model.ID, displayName)
	}
	return models, nil
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func redactDiscoveryError(message string, key providerauth.Key, headers map[string]string) string {
	message = redactSecret(message, key.APIKey)
	message = redactSecret(message, key.ProxyURL)
	for _, value := range headers {
		message = redactSecret(message, value)
	}
	return message
}

type testConnectionInput struct {
	Config config.Config `json:"config"`
	Key    editableKey   `json:"key"`
	Model  string        `json:"model"`
}

func (a *API) testConnection(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	if a.tester == nil {
		return jsonError(http.StatusNotImplemented, "test_unavailable", "connection testing is unavailable")
	}
	var input testConnectionInput
	if err := decodeJSON(body, &input); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_body", err.Error())
	}
	input.Config.Normalize()
	if err := input.Config.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_config", err.Error())
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Model == "" {
		return jsonError(http.StatusBadRequest, "model_required", "test model is required")
	}
	model, ok := input.Config.ResolveModel(input.Model)
	if !ok {
		return jsonError(http.StatusBadRequest, "model_not_configured", "test model is not configured")
	}
	if model.Image {
		return jsonError(http.StatusBadRequest, "image_model_not_testable", "image models cannot be used by the connection test")
	}
	key, failure, ok := a.resolveDraftKey(ctx, input.Config, input.Key, "connection test")
	if !ok {
		return failure
	}
	started := time.Now()
	result, err := a.tester.TestConnection(ctx, TestRequest{
		Config:     input.Config,
		Key:        key,
		Model:      input.Model,
		HTTPClient: httpClientFromContext(ctx),
	})
	if err != nil {
		message := redactSecret(err.Error(), key.APIKey)
		message = redactSecret(message, key.ProxyURL)
		if strings.TrimSpace(message) == "" {
			message = "connection test failed"
		}
		return jsonError(http.StatusBadGateway, "connection_test_failed", message)
	}
	if result.LatencyMS <= 0 {
		result.LatencyMS = time.Since(started).Milliseconds()
	}
	if result.Message == "" {
		result.Message = "连接成功"
	}
	if result.Model == "" {
		result.Model = input.Model
	}
	return jsonResponse(http.StatusOK, result)
}

func (a *API) resolveDraftKey(ctx context.Context, draftConfig config.Config, draft editableKey, operation string) (providerauth.Key, pluginapi.ManagementResponse, bool) {
	key := draft.authKey()
	usesStoredCredential := strings.TrimSpace(key.APIKey) == "" || draft.PreserveProxyURL
	if usesStoredCredential && !sameCredentialDestination(a.config, draftConfig) {
		return providerauth.Key{}, jsonError(
			http.StatusBadRequest,
			"credential_reentry_required",
			"re-enter the API key and proxy URL when using a changed provider destination for "+operation,
		), false
	}
	if usesStoredCredential {
		credential, err := a.loadCredential(ctx)
		if err != nil {
			return providerauth.Key{}, jsonError(http.StatusInternalServerError, "credential_read_failed", "failed to read provider credentials"), false
		}
		if credential.Destination == nil || !credential.Destination.MatchesConfig(draftConfig) {
			return providerauth.Key{}, jsonError(
				http.StatusBadRequest,
				"credential_reentry_required",
				"re-enter the API key and proxy URL because the stored credential belongs to a different provider destination",
			), false
		}
		found := false
		for _, stored := range credential.Keys {
			if stored.ID != key.ID {
				continue
			}
			found = true
			if strings.TrimSpace(key.APIKey) == "" {
				key.APIKey = stored.APIKey
			}
			if draft.PreserveProxyURL {
				key.ProxyURL = stored.ProxyURL
			}
			break
		}
		if draft.PreserveProxyURL && !found {
			return providerauth.Key{}, jsonError(http.StatusBadRequest, "invalid_key", "preserve-proxy-url requires an existing key"), false
		}
	}
	if strings.TrimSpace(key.APIKey) == "" {
		return providerauth.Key{}, jsonError(http.StatusBadRequest, "missing_secret", "an API key is required for "+operation), false
	}
	if err := validateEditableKey(key, 0); err != nil {
		return providerauth.Key{}, jsonError(http.StatusBadRequest, "invalid_key", err.Error()), false
	}
	return key, pluginapi.ManagementResponse{}, true
}

func viewKey(key providerauth.Key) keyView {
	proxyURL, proxySecretPresent := providerauth.DisplayProxyURL(key.ProxyURL)
	return keyView{
		ID: key.ID, Label: key.Label, Masked: providerauth.MaskAPIKey(key.APIKey),
		SecretPresent: key.APIKey != "", ProxyURL: proxyURL, ProxySecretPresent: proxySecretPresent,
		Priority: key.Priority, Disabled: key.Disabled,
	}
}

func keyViews(keys []providerauth.Key) []keyView {
	views := make([]keyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, viewKey(key))
	}
	return views
}

func sameCredentialDestination(saved, draft config.Config) bool {
	saved.Normalize()
	draft.Normalize()
	return saved.Protocol == draft.Protocol && saved.BaseURL != "" && saved.BaseURL == draft.BaseURL
}

func (a *API) loadCredential(ctx context.Context) (providerauth.CredentialFile, error) {
	empty := providerauth.CredentialFile{Type: a.pluginID, Keys: []providerauth.Key{}}
	if a.authStore == nil {
		return empty, nil
	}
	entries, err := a.authStore.ListAuth(ctx)
	if err != nil {
		return providerauth.CredentialFile{}, err
	}
	var fallback *pluginapi.HostAuthFileEntry
	for index := range entries {
		entry := entries[index]
		matchesProvider := strings.EqualFold(strings.TrimSpace(entry.Provider), a.pluginID) ||
			strings.EqualFold(strings.TrimSpace(entry.Type), a.pluginID)
		matchesFile := strings.EqualFold(strings.TrimSpace(entry.Name), a.credentialFile)
		if !matchesProvider && !matchesFile {
			continue
		}
		if fallback == nil {
			copyEntry := entry
			fallback = &copyEntry
		}
		if matchesFile && strings.TrimSpace(entry.AuthIndex) != "" {
			fallback = &entry
			break
		}
	}
	if fallback == nil || strings.TrimSpace(fallback.AuthIndex) == "" {
		return empty, nil
	}
	got, err := a.authStore.GetAuth(ctx, pluginapi.HostAuthGetRequest{AuthIndex: fallback.AuthIndex})
	if err != nil {
		return providerauth.CredentialFile{}, err
	}
	credential, handled, err := providerauth.ParseCredentialFile(got.JSON)
	if err != nil {
		return providerauth.CredentialFile{}, err
	}
	if !handled {
		return providerauth.CredentialFile{}, errors.New("credential file belongs to another provider")
	}
	return credential, nil
}

func validateEditableKey(key providerauth.Key, index int) error {
	if len(key.ID) > 128 || strings.Contains(key.ID, "#") {
		return fmt.Errorf("keys[%d].id is invalid", index)
	}
	for _, r := range key.ID {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("keys[%d].id is invalid", index)
		}
	}
	if len(key.Label) > 128 {
		return fmt.Errorf("keys[%d].label is too long", index)
	}
	if len(key.APIKey) > 16*1024 {
		return fmt.Errorf("keys[%d].api-key is too long", index)
	}
	if key.Priority < -1_000_000 || key.Priority > 1_000_000 {
		return fmt.Errorf("keys[%d].priority is out of range", index)
	}
	probe := providerauth.CredentialFile{Type: provider.ID, Keys: []providerauth.Key{key}}
	_, err := providerauth.MarshalCredentialFile(probe)
	return err
}

func randomKeyID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err == nil {
		return "key-" + hex.EncodeToString(raw)
	}
	return fmt.Sprintf("key-%d", time.Now().UnixNano())
}

func decodeJSON(body []byte, target any) error {
	if len(body) == 0 {
		return errors.New("request body is required")
	}
	if len(body) > maxRequestBody {
		return errors.New("request body is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func redactSecret(message, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[REDACTED]")
}

func resourceResponse() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"Content-Security-Policy": []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
			"Referrer-Policy":         []string{"no-referrer"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Permissions-Policy":      []string{"camera=(), microphone=(), geolocation=()"},
		},
		Body: append([]byte(nil), ui.ProviderPage...),
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "response_encode_failed", "failed to encode response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

func jsonError(status int, code, message string) pluginapi.ManagementResponse {
	return jsonResponse(status, map[string]string{"error": code, "message": message})
}
