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
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/ui"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	maxRequestBody = 2 << 20
	maxKeys        = 128
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
			{Method: http.MethodPut, Path: base + "/keys", Description: "Persists the provider credential pool.", Handler: a},
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
	case req.Method == http.MethodPut && strings.HasSuffix(path, "/plugins/"+a.pluginID+"/keys"):
		return a.saveKeys(ctx, req.Body), nil
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
	if existingRaw, errExisting := providerauth.MarshalCredentialFile(existing); errExisting == nil && bytes.Equal(existingRaw, raw) {
		return jsonResponse(http.StatusOK, map[string]any{"status": "ok", "keys": keyViews(keys)})
	}
	if _, err = a.authStore.SaveAuth(ctx, pluginapi.HostAuthSaveRequest{
		Name: a.credentialFile,
		JSON: json.RawMessage(raw),
	}); err != nil {
		return jsonError(http.StatusInternalServerError, "credential_save_failed", "failed to save provider credentials")
	}
	return jsonResponse(http.StatusOK, map[string]any{"status": "ok", "keys": keyViews(keys)})
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
	key := input.Key.authKey()
	usesStoredCredential := strings.TrimSpace(key.APIKey) == "" || input.Key.PreserveProxyURL
	if usesStoredCredential && !sameCredentialDestination(a.config, input.Config) {
		return jsonError(http.StatusBadRequest, "credential_reentry_required", "re-enter the API key and proxy URL when testing a changed provider destination")
	}
	if usesStoredCredential {
		credential, err := a.loadCredential(ctx)
		if err != nil {
			return jsonError(http.StatusInternalServerError, "credential_read_failed", "failed to read provider credentials")
		}
		if credential.Destination == nil || !credential.Destination.MatchesConfig(input.Config) {
			return jsonError(http.StatusBadRequest, "credential_reentry_required", "re-enter the API key and proxy URL because the stored credential belongs to a different provider destination")
		}
		found := false
		for _, stored := range credential.Keys {
			if stored.ID == key.ID {
				found = true
				if strings.TrimSpace(key.APIKey) == "" {
					key.APIKey = stored.APIKey
				}
				if input.Key.PreserveProxyURL {
					key.ProxyURL = stored.ProxyURL
				}
				break
			}
		}
		if input.Key.PreserveProxyURL && !found {
			return jsonError(http.StatusBadRequest, "invalid_key", "preserve-proxy-url requires an existing key")
		}
	}
	if strings.TrimSpace(key.APIKey) == "" {
		return jsonError(http.StatusBadRequest, "missing_secret", "an API key is required for the connection test")
	}
	if err := validateEditableKey(key, 0); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
	}
	if strings.TrimSpace(key.ProxyURL) != "" {
		return jsonError(http.StatusUnprocessableEntity, "proxy_test_unsupported", "connection testing cannot safely exercise a per-key proxy; save the key and verify it through a normal model request")
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
