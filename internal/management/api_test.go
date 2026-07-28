package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeAuthStore struct {
	raw       []byte
	saveCalls int
	updatedAt time.Time
}

func (s *fakeAuthStore) ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if len(s.raw) == 0 {
		return nil, nil
	}
	return fakeAuthEntriesAt(s.raw, s.updatedAt), nil
}

func fakeAuthEntries(raw []byte) []pluginapi.HostAuthFileEntry {
	return fakeAuthEntriesAt(raw, time.Time{})
}

func fakeAuthEntriesAt(raw []byte, updatedAt time.Time) []pluginapi.HostAuthFileEntry {
	entries := []pluginapi.HostAuthFileEntry{{
		ID:        provider.CredentialsFile,
		AuthIndex: "auth-index-1",
		Name:      provider.CredentialsFile,
		Type:      provider.ID,
		Provider:  provider.ID,
		Disabled:  true,
		UpdatedAt: updatedAt,
	}}
	credential, handled, err := providerauth.ParseCredentialFile(raw)
	if err != nil || !handled {
		return entries
	}
	for _, key := range credential.Keys {
		entries = append(entries, pluginapi.HostAuthFileEntry{
			ID:        provider.CredentialsFile + "#" + key.ID,
			AuthIndex: "auth-index-" + key.ID,
			Name:      provider.CredentialsFile,
			Type:      provider.ID,
			Provider:  provider.ID,
			Disabled:  key.Disabled,
			UpdatedAt: updatedAt,
		})
	}
	return entries
}

func (s *fakeAuthStore) GetAuth(_ context.Context, req pluginapi.HostAuthGetRequest) (pluginapi.HostAuthGetResponse, error) {
	if req.AuthIndex != "auth-index-1" || len(s.raw) == 0 {
		return pluginapi.HostAuthGetResponse{}, errors.New("credential not found")
	}
	return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, Name: provider.CredentialsFile, JSON: append([]byte(nil), s.raw...)}, nil
}

func (s *fakeAuthStore) SaveAuth(_ context.Context, req pluginapi.HostAuthSaveRequest) (pluginapi.HostAuthSaveResponse, error) {
	s.saveCalls++
	s.raw = append([]byte(nil), req.JSON...)
	s.updatedAt = time.Now().UTC()
	return pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name}, nil
}

type fakeTester struct {
	request TestRequest
	result  TestResult
	err     error
	calls   int
}

type fakeHTTPClient struct {
	request  pluginapi.HTTPRequest
	response pluginapi.HTTPResponse
	err      error
	calls    int
}

func (c *fakeHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	c.calls++
	c.request = req
	return c.response, c.err
}

func (c *fakeHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming is not supported by this test client")
}

func disabledConfig() config.Config {
	cfg := config.Default()
	cfg.BaseURL = "https://saved.example.com/v1"
	cfg.Disabled = true
	return cfg
}

func saveKeysBody(t *testing.T, destination config.Config, body string) []byte {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode save body: %v", err)
	}
	payload["destination"] = map[string]any{
		"protocol": destination.Protocol,
		"base-url": destination.BaseURL,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode save body: %v", err)
	}
	return raw
}

func sameDestinationBody(t *testing.T, body string) []byte {
	t.Helper()
	return saveKeysBody(t, disabledConfig(), body)
}

func (t *fakeTester) TestConnection(_ context.Context, req TestRequest) (TestResult, error) {
	t.calls++
	t.request = req
	return t.result, t.err
}

func TestRegisterManagementDeclaresAuthenticatedAndResourceRoutes(t *testing.T) {
	t.Parallel()
	api := New(provider.ID, provider.CredentialsFile, config.Default(), nil, nil)
	registration, err := api.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{})
	if err != nil {
		t.Fatalf("RegisterManagement() error = %v", err)
	}
	if len(registration.Routes) != 5 || len(registration.Resources) != 1 {
		t.Fatalf("registration = %#v, want five routes and one resource", registration)
	}
	for _, route := range registration.Routes {
		if route.Menu != "" || route.Handler != api {
			t.Fatalf("authenticated route = %#v", route)
		}
	}
	resource := registration.Resources[0]
	if resource.Path != "/provider" || resource.Menu == "" || resource.Handler != api {
		t.Fatalf("resource = %#v", resource)
	}
}

func TestResourceIsStaticAndContainsNoRuntimeConfiguration(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Name = "do-not-render-provider-name"
	api := New(provider.ID, provider.CredentialsFile, cfg, nil, nil)
	resp, err := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + provider.ID + "/provider",
	})
	if err != nil {
		t.Fatalf("HandleManagement() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(resp.Body, []byte("<!doctype html>")) {
		t.Fatalf("resource response = status %d, body %q", resp.StatusCode, resp.Body)
	}
	if bytes.Contains(resp.Body, []byte(cfg.Name)) {
		t.Fatal("resource shell contains runtime provider configuration")
	}
	if got := resp.Headers.Get("Content-Security-Policy"); !stringsContain(got, "connect-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	if got := resp.Headers.Get("Content-Security-Policy"); !stringsContain(got, "frame-ancestors 'self'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	for _, fragment := range []string{
		"id=\"discover-models\"", "`${API}/validate`", "`${API}/models`", "beforeunload", "waitUntilConfigMatches",
		"method: 'PUT'", "headers: Object.keys(config.headers || {}).length ? config.headers : null", "pointer-events: none",
	} {
		if !bytes.Contains(resp.Body, []byte(fragment)) {
			t.Fatalf("resource shell is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"sessionStorage", "localStorage", "setTimeout(load"} {
		if bytes.Contains(resp.Body, []byte(forbidden)) {
			t.Fatalf("resource shell contains forbidden client persistence/race pattern %q", forbidden)
		}
	}
}

func TestValidateConfigNormalizesDraft(t *testing.T) {
	t.Parallel()
	api := New(provider.ID, provider.CredentialsFile, config.Default(), nil, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/validate",
		Body:   []byte(`{"config":{"name":"  Draft Provider  ","protocol":"GEMINI","base-url":"https://api.example.com/v1/","prefix":"/team/","models":[{"name":" model-one "}]}}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	var body struct {
		Config config.Config `json:"config"`
	}
	decodeResponse(t, resp, &body)
	if body.Config.Name != "Draft Provider" || body.Config.Protocol != config.ProtocolGemini || body.Config.BaseURL != "https://api.example.com/v1" || body.Config.Prefix != "team" {
		t.Fatalf("normalized config = %#v", body.Config)
	}
	if len(body.Config.Models) != 1 || body.Config.Models[0].Name != "model-one" {
		t.Fatalf("normalized models = %#v", body.Config.Models)
	}
}

func TestValidateConfigPreservesExplicitZeroValuesForHostPatch(t *testing.T) {
	t.Parallel()
	api := New(provider.ID, provider.CredentialsFile, config.Default(), nil, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/validate",
		Body:   []byte(`{"config":{"name":"Provider","priority":0,"protocol":"openai-chat-completions","base-url":"https://api.example.com/v1","prefix":"","disabled":false,"disable-cooling":false,"headers":{},"models":[],"test-model":""}}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	for _, fragment := range []string{
		`"priority":0`, `"prefix":""`, `"disabled":false`, `"disable-cooling":false`,
		`"headers":{}`, `"models":[]`, `"test-model":""`,
	} {
		if !bytes.Contains(resp.Body, []byte(fragment)) {
			t.Fatalf("validated config omitted %s: %s", fragment, resp.Body)
		}
	}
}

func TestValidateConfigRejectsInvalidThinkingRange(t *testing.T) {
	t.Parallel()
	api := New(provider.ID, provider.CredentialsFile, config.Default(), nil, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/validate",
		Body:   []byte(`{"config":{"protocol":"openai-chat-completions","base-url":"https://api.example.com/v1","models":[{"name":"model-one","thinking":{"min":20,"max":10}}]}}`),
	})
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(resp.Body, []byte("invalid_config")) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
}

func TestStateMasksCredentials(t *testing.T) {
	t.Parallel()
	const secret = "sk-secret-value-123456"
	const proxySecret = "proxy-password"
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{
		ID: "primary", Label: "Primary", APIKey: secret, ProxyURL: "https://proxy-user:" + proxySecret + "@proxy.example:8443/private?token=hidden", Priority: 4,
	}})}
	cfg := config.Default()
	cfg.Priority = 17
	api := New(provider.ID, provider.CredentialsFile, cfg, store, nil)
	resp, err := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + provider.ID + "/state",
	})
	if err != nil {
		t.Fatalf("HandleManagement() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if bytes.Contains(resp.Body, []byte(secret)) || bytes.Contains(resp.Body, []byte(proxySecret)) || bytes.Contains(resp.Body, []byte("proxy-user")) || bytes.Contains(resp.Body, []byte("private")) {
		t.Fatal("state response contains plaintext API or proxy credentials")
	}
	var body struct {
		Config                config.Config                       `json:"config"`
		CredentialDestination *providerauth.CredentialDestination `json:"credential-destination"`
		Keys                  []keyView                           `json:"keys"`
	}
	decodeResponse(t, resp, &body)
	if len(body.Keys) != 1 || !body.Keys[0].SecretPresent || body.Keys[0].Masked == "" {
		t.Fatalf("keys = %#v", body.Keys)
	}
	if !body.Keys[0].ProxySecretPresent || body.Keys[0].ProxyURL != "https://redacted@proxy.example:8443" {
		t.Fatalf("proxy view = %#v", body.Keys[0])
	}
	if body.Config.Priority != 17 {
		t.Fatalf("config priority = %d, want 17", body.Config.Priority)
	}
	if body.CredentialDestination == nil || !body.CredentialDestination.MatchesConfig(disabledConfig()) {
		t.Fatalf("credential destination = %#v", body.CredentialDestination)
	}
}

func TestSaveKeysPreservesClearsAndReplacesAuthenticatedProxy(t *testing.T) {
	t.Parallel()
	const original = "http://proxy-user:proxy-password@proxy.example:8080"
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "preserve redacted display",
			body: `{"keys":[{"id":"primary","api-key":"","proxy-url":"http://redacted@proxy.example:8080","preserve-proxy-url":true}]}`,
			want: original,
		},
		{
			name: "clear",
			body: `{"keys":[{"id":"primary","api-key":"","proxy-url":""}]}`,
			want: "",
		},
		{
			name: "replace",
			body: `{"keys":[{"id":"primary","api-key":"","proxy-url":"socks5h://new-user:new-password@proxy.example:1080"}]}`,
			want: "socks5h://new-user:new-password@proxy.example:1080",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", APIKey: "sk-existing", ProxyURL: original}})}
			api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPut, Path: "/v0/management/plugins/" + provider.ID + "/keys", Body: sameDestinationBody(t, test.body),
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
			}
			credential, handled, err := providerauth.ParseCredentialFile(store.raw)
			if err != nil || !handled || credential.Keys[0].ProxyURL != test.want {
				t.Fatalf("saved credential = %#v, handled = %v, error = %v", credential, handled, err)
			}
			if bytes.Contains(resp.Body, []byte("proxy-password")) || bytes.Contains(resp.Body, []byte("new-password")) {
				t.Fatalf("save response leaked proxy credentials: %s", resp.Body)
			}
		})
	}
}

func TestDecodeJSONRejectsTrailingValue(t *testing.T) {
	t.Parallel()
	var target map[string]any
	if err := decodeJSON([]byte(`{} {}`), &target); err == nil {
		t.Fatal("decodeJSON() accepted trailing JSON")
	}
}

func TestSaveKeysPreservesBlankExistingSecret(t *testing.T) {
	t.Parallel()
	const secret = "sk-secret-value-123456"
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", Label: "Old", APIKey: secret}})}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	body := sameDestinationBody(t, `{"keys":[{"id":"primary","label":"Renamed","api-key":"","proxy-url":"https://proxy.example","priority":2}]}`)
	resp, err := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   body,
	})
	if err != nil {
		t.Fatalf("HandleManagement() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || store.saveCalls != 1 {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
	credential, handled, err := providerauth.ParseCredentialFile(store.raw)
	if err != nil || !handled {
		t.Fatalf("ParseCredentialFile() = handled %v, error %v", handled, err)
	}
	if got := credential.Keys[0]; got.APIKey != secret || got.Label != "Renamed" || got.ProxyURL != "https://proxy.example" || got.Priority != 2 {
		t.Fatalf("saved key = %#v", got)
	}
	if bytes.Contains(resp.Body, []byte(secret)) {
		t.Fatal("save response contains the plaintext API key")
	}
}

func TestSaveKeysRejectsBlankNewSecret(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"new-key","api-key":""}]}`),
	})
	if resp.StatusCode != http.StatusBadRequest || store.saveCalls != 0 {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
}

func TestSaveKeysSkipsByteEquivalentCredentialWrite(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", Label: "Primary", APIKey: "sk-existing", ProxyURL: "direct"}})}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"primary","label":"Primary","api-key":"","proxy-url":"direct"}]}`),
	})
	if resp.StatusCode != http.StatusOK || store.saveCalls != 0 {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
}

func TestSaveKeysAllowsEmptyPool(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "old", APIKey: "sk-old-secret"}})}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[]}`),
	})
	if resp.StatusCode != http.StatusOK || store.saveCalls != 1 {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
	credential, handled, err := providerauth.ParseCredentialFile(store.raw)
	if err != nil || !handled || len(credential.Keys) != 0 {
		t.Fatalf("saved credential = %#v, handled = %v, error = %v", credential, handled, err)
	}
}

func TestSaveKeysRequiresDestination(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   []byte(`{"keys":[]}`),
	})
	if resp.StatusCode != http.StatusBadRequest || store.saveCalls != 0 || !bytes.Contains(resp.Body, []byte("invalid_destination")) {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
}

func TestSaveKeysRequiresCredentialReentryForChangedDestination(t *testing.T) {
	t.Parallel()
	saved := disabledConfig()
	draft := saved
	draft.BaseURL = "https://new.example.com/v1"
	const storedProxy = "http://proxy-user:proxy-password@proxy.example:8080"

	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "hidden API key",
			body: `{"keys":[{"id":"primary","api-key":"","proxy-url":""}]}`,
		},
		{
			name: "preserved authenticated proxy",
			body: `{"keys":[{"id":"primary","api-key":"new-secret","proxy-url":"http://redacted@proxy.example:8080","preserve-proxy-url":true}]}`,
		},
		{
			name: "redacted authenticated proxy without preserve flag",
			body: `{"keys":[{"id":"primary","api-key":"new-secret","proxy-url":"http://redacted@proxy.example:8080"}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", APIKey: "old-secret", ProxyURL: storedProxy}})}
			api := New(provider.ID, provider.CredentialsFile, saved, store, nil)
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPut,
				Path:   "/v0/management/plugins/" + provider.ID + "/keys",
				Body:   saveKeysBody(t, draft, test.body),
			})
			if resp.StatusCode != http.StatusBadRequest || store.saveCalls != 0 || !bytes.Contains(resp.Body, []byte("credential_reentry_required")) {
				t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
			}
			if bytes.Contains(resp.Body, []byte("old-secret")) || bytes.Contains(resp.Body, []byte("proxy-password")) {
				t.Fatalf("response leaked stored credentials: %s", resp.Body)
			}
		})
	}
}

func TestSaveKeysRequiresCredentialReentryForLegacyUnboundFile(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{raw: []byte(`{"type":"multi-protocol-provider","keys":[{"id":"primary","api-key":"legacy-secret"}]}`)}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"primary","api-key":""}]}`),
	})
	if resp.StatusCode != http.StatusBadRequest || store.saveCalls != 0 || !bytes.Contains(resp.Body, []byte("credential_reentry_required")) {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
	if bytes.Contains(resp.Body, []byte("legacy-secret")) {
		t.Fatalf("response leaked legacy credential: %s", resp.Body)
	}
}

func TestSaveKeysAcceptsExplicitCredentialsForChangedDestination(t *testing.T) {
	t.Parallel()
	saved := disabledConfig()
	draft := saved
	draft.Protocol = config.ProtocolAnthropic
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", APIKey: "old-secret", ProxyURL: "http://old:secret@proxy.example:8080"}})}
	api := New(provider.ID, provider.CredentialsFile, saved, store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body: saveKeysBody(t, draft,
			`{"keys":[{"id":"primary","api-key":"new-secret","proxy-url":"socks5h://new-user:new-password@proxy.example:1080"}]}`),
	})
	if resp.StatusCode != http.StatusOK || store.saveCalls != 1 {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
	credential, handled, err := providerauth.ParseCredentialFile(store.raw)
	if err != nil || !handled || len(credential.Keys) != 1 {
		t.Fatalf("saved credential = %#v, handled = %v, error = %v", credential, handled, err)
	}
	if got := credential.Keys[0]; got.APIKey != "new-secret" || got.ProxyURL != "socks5h://new-user:new-password@proxy.example:1080" {
		t.Fatalf("saved key = %#v", got)
	}
	if credential.Destination == nil || !credential.Destination.MatchesConfig(draft) {
		t.Fatalf("saved destination = %#v, want %#v", credential.Destination, draft)
	}
}

func TestSaveKeysCannotRebindHiddenSecretAfterIncompleteConfigCommit(t *testing.T) {
	t.Parallel()
	saved := disabledConfig()
	draft := saved
	draft.BaseURL = "https://new.example.com/v1"
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", APIKey: "old-secret"}})}

	firstAPI := New(provider.ID, provider.CredentialsFile, saved, store, nil)
	first, _ := firstAPI.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   saveKeysBody(t, draft, `{"keys":[{"id":"primary","api-key":"new-destination-secret"}]}`),
	})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first save status = %d, body = %s", first.StatusCode, first.Body)
	}

	// Simulate the final config PATCH failing: CPA still has the old disabled
	// destination, while the credential file is already bound to the new one.
	secondAPI := New(provider.ID, provider.CredentialsFile, saved, store, nil)
	second, _ := secondAPI.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   saveKeysBody(t, saved, `{"keys":[{"id":"primary","api-key":""}]}`),
	})
	if second.StatusCode != http.StatusBadRequest || store.saveCalls != 1 || !bytes.Contains(second.Body, []byte("credential_reentry_required")) {
		t.Fatalf("second save status = %d, saves = %d, body = %s", second.StatusCode, store.saveCalls, second.Body)
	}
}

func TestSaveKeysRejectsCredentialChangesWhileProviderIsEnabled(t *testing.T) {
	t.Parallel()
	store := &fakeAuthStore{}
	api := New(provider.ID, provider.CredentialsFile, config.Default(), store, nil)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   []byte(`{"keys":[{"id":"new-key","api-key":"sk-secret"}]}`),
	})
	if resp.StatusCode != http.StatusConflict || store.saveCalls != 0 || !bytes.Contains(resp.Body, []byte("provider_must_be_disabled")) {
		t.Fatalf("status = %d, saves = %d, body = %s", resp.StatusCode, store.saveCalls, resp.Body)
	}
}

type delayedAuthStore struct {
	fakeAuthStore
	staleRaw           []byte
	staleUpdatedAt     time.Time
	postSaveListCalls  int
	convergeAfterPolls int
	neverConverge      bool
	saved              bool
}

func (s *delayedAuthStore) ListAuth(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if !s.saved {
		return s.fakeAuthStore.ListAuth(ctx)
	}
	s.postSaveListCalls++
	if s.neverConverge || s.postSaveListCalls <= s.convergeAfterPolls {
		if len(s.staleRaw) == 0 {
			return nil, nil
		}
		return fakeAuthEntriesAt(s.staleRaw, s.staleUpdatedAt), nil
	}
	return s.fakeAuthStore.ListAuth(ctx)
}

func (s *delayedAuthStore) SaveAuth(ctx context.Context, req pluginapi.HostAuthSaveRequest) (pluginapi.HostAuthSaveResponse, error) {
	s.staleRaw = append([]byte(nil), s.raw...)
	s.staleUpdatedAt = s.updatedAt
	response, err := s.fakeAuthStore.SaveAuth(ctx, req)
	s.saved = err == nil
	return response, err
}

func TestSaveKeysWaitsForExactAuthConvergence(t *testing.T) {
	store := &delayedAuthStore{
		fakeAuthStore:      fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "deleted", APIKey: "sk-old"}})},
		convergeAfterPolls: 2,
	}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	api.authSyncWait = time.Second
	api.authSyncPoll = time.Millisecond
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"current","api-key":"sk-current","disabled":true}]}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if store.postSaveListCalls < 3 {
		t.Fatalf("post-save list calls = %d, want at least 3", store.postSaveListCalls)
	}
}

func TestSaveKeysWaitsForFreshSameIDKeyUpdate(t *testing.T) {
	oldTime := time.Now().Add(-time.Hour).UTC()
	store := &delayedAuthStore{
		fakeAuthStore: fakeAuthStore{
			raw:       mustCredential(t, []providerauth.Key{{ID: "current", APIKey: "sk-old"}}),
			updatedAt: oldTime,
		},
		convergeAfterPolls: 2,
	}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	api.authSyncWait = time.Second
	api.authSyncPoll = time.Millisecond
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"current","api-key":"sk-new"}]}`),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if store.postSaveListCalls < 3 {
		t.Fatalf("post-save list calls = %d, want at least 3 for a fresh same-ID record", store.postSaveListCalls)
	}
}

func TestAuthEntriesConvergedRequiresDisabledControllerAndExactKeySet(t *testing.T) {
	t.Parallel()
	expected := map[string]bool{
		provider.CredentialsFile + "#primary": false,
	}
	ready := []pluginapi.HostAuthFileEntry{
		{ID: provider.CredentialsFile, Disabled: true},
		{ID: provider.CredentialsFile + "#primary"},
		{ID: "unrelated.json#other"},
	}
	if !authEntriesConverged(ready, provider.CredentialsFile, expected, time.Time{}) {
		t.Fatal("exact controller/key state did not converge")
	}
	for name, entries := range map[string][]pluginapi.HostAuthFileEntry{
		"enabled controller": {
			{ID: provider.CredentialsFile, Disabled: false},
			{ID: provider.CredentialsFile + "#primary"},
		},
		"missing key": {
			{ID: provider.CredentialsFile, Disabled: true},
		},
		"wrong key disabled state": {
			{ID: provider.CredentialsFile, Disabled: true},
			{ID: provider.CredentialsFile + "#primary", Disabled: true},
		},
		"stale key": {
			{ID: provider.CredentialsFile, Disabled: true},
			{ID: provider.CredentialsFile + "#primary"},
			{ID: provider.CredentialsFile + "#deleted"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if authEntriesConverged(entries, provider.CredentialsFile, expected, time.Time{}) {
				t.Fatalf("entries unexpectedly converged: %#v", entries)
			}
		})
	}
	freshCutoff := time.Now().UTC()
	stale := []pluginapi.HostAuthFileEntry{
		{ID: provider.CredentialsFile, Disabled: true, UpdatedAt: freshCutoff.Add(-time.Second)},
		{ID: provider.CredentialsFile + "#primary", UpdatedAt: freshCutoff.Add(-time.Second)},
	}
	if authEntriesConverged(stale, provider.CredentialsFile, expected, freshCutoff) {
		t.Fatal("stale same-ID records unexpectedly converged")
	}
	fresh := []pluginapi.HostAuthFileEntry{
		{ID: provider.CredentialsFile, Disabled: true, UpdatedAt: freshCutoff},
		{ID: provider.CredentialsFile + "#primary", UpdatedAt: freshCutoff},
	}
	if !authEntriesConverged(fresh, provider.CredentialsFile, expected, freshCutoff) {
		t.Fatal("fresh exact controller/key state did not converge")
	}
}

func TestSaveKeysReportsAuthConvergenceTimeout(t *testing.T) {
	store := &delayedAuthStore{neverConverge: true}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	api.authSyncWait = 20 * time.Millisecond
	api.authSyncPoll = time.Millisecond
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/" + provider.ID + "/keys",
		Body:   sameDestinationBody(t, `{"keys":[{"id":"current","api-key":"sk-current"}]}`),
	})
	if resp.StatusCode != http.StatusGatewayTimeout || !bytes.Contains(resp.Body, []byte("auth_reconcile_timeout")) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if store.saveCalls != 1 {
		t.Fatalf("save calls = %d, want 1", store.saveCalls)
	}
}

type blockingSaveAuthStore struct {
	mu                sync.Mutex
	raw               []byte
	updatedAt         time.Time
	saveCalls         int
	firstSaveStarted  chan struct{}
	releaseFirstSave  chan struct{}
	secondSaveStarted chan struct{}
}

func (s *blockingSaveAuthStore) ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.raw) == 0 {
		return nil, nil
	}
	return fakeAuthEntriesAt(s.raw, s.updatedAt), nil
}

func (s *blockingSaveAuthStore) GetAuth(context.Context, pluginapi.HostAuthGetRequest) (pluginapi.HostAuthGetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pluginapi.HostAuthGetResponse{AuthIndex: "auth-index-1", Name: provider.CredentialsFile, JSON: append([]byte(nil), s.raw...)}, nil
}

func (s *blockingSaveAuthStore) SaveAuth(_ context.Context, req pluginapi.HostAuthSaveRequest) (pluginapi.HostAuthSaveResponse, error) {
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	s.mu.Unlock()
	if call == 1 {
		close(s.firstSaveStarted)
		<-s.releaseFirstSave
	} else if call == 2 {
		close(s.secondSaveStarted)
	}
	s.mu.Lock()
	s.raw = append([]byte(nil), req.JSON...)
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
	return pluginapi.HostAuthSaveResponse{Name: req.Name}, nil
}

func TestConcurrentCredentialSavesAreSerialized(t *testing.T) {
	store := &blockingSaveAuthStore{
		firstSaveStarted:  make(chan struct{}),
		releaseFirstSave:  make(chan struct{}),
		secondSaveStarted: make(chan struct{}),
	}
	api := New(provider.ID, provider.CredentialsFile, disabledConfig(), store, nil)
	save := func(id string) <-chan pluginapi.ManagementResponse {
		body := sameDestinationBody(t, `{"keys":[{"id":"`+id+`","api-key":"sk-`+id+`"}]}`)
		result := make(chan pluginapi.ManagementResponse, 1)
		go func() {
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPut,
				Path:   "/v0/management/plugins/" + provider.ID + "/keys",
				Body:   body,
			})
			result <- resp
		}()
		return result
	}

	first := save("first")
	<-store.firstSaveStarted
	second := save("second")
	released := false
	defer func() {
		if !released {
			close(store.releaseFirstSave)
		}
	}()
	select {
	case <-store.secondSaveStarted:
		t.Fatal("second credential save entered the auth store before the first completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(store.releaseFirstSave)
	released = true
	for index, result := range []<-chan pluginapi.ManagementResponse{first, second} {
		select {
		case resp := <-result:
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("save %d status = %d, body = %s", index+1, resp.StatusCode, resp.Body)
			}
		case <-time.After(time.Second):
			t.Fatalf("save %d did not complete", index+1)
		}
	}
}

func TestDiscoverModelsUsesProtocolAuthenticationAndParsesResponses(t *testing.T) {
	tests := []struct {
		name         string
		protocol     config.Protocol
		responseBody string
		authHeader   string
		authValue    string
		wantName     string
		wantDisplay  string
	}{
		{
			name: "OpenAI Chat Completions", protocol: config.ProtocolOpenAIChat,
			responseBody: `{"data":[{"id":"gpt-compatible","name":"Compatible GPT"}]}`,
			authHeader:   "Authorization", authValue: "Bearer sk-discovery",
			wantName: "gpt-compatible", wantDisplay: "Compatible GPT",
		},
		{
			name: "OpenAI Responses", protocol: config.ProtocolOpenAIResponses,
			responseBody: `{"data":[{"id":"response-model","display_name":"Response Model"}]}`,
			authHeader:   "Authorization", authValue: "Bearer sk-discovery",
			wantName: "response-model", wantDisplay: "Response Model",
		},
		{
			name: "Anthropic Messages", protocol: config.ProtocolAnthropic,
			responseBody: `{"data":[{"id":"claude-compatible","display_name":"Claude Compatible"}]}`,
			authHeader:   "X-Api-Key", authValue: "sk-discovery",
			wantName: "claude-compatible", wantDisplay: "Claude Compatible",
		},
		{
			name: "Gemini", protocol: config.ProtocolGemini,
			responseBody: `{"models":[{"name":"models/gemini-compatible","displayName":"Gemini Compatible","supportedGenerationMethods":["generateContent"]},{"name":"models/embedding-only","supportedGenerationMethods":["embedContent"]}]}`,
			authHeader:   "X-Goog-Api-Key", authValue: "sk-discovery",
			wantName: "gemini-compatible", wantDisplay: "Gemini Compatible",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Protocol = test.protocol
			cfg.BaseURL = "https://draft.example.com/v1"
			body, err := json.Marshal(map[string]any{
				"config": cfg,
				"key":    map[string]any{"id": "draft", "api-key": "sk-discovery"},
			})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			client := &fakeHTTPClient{response: pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(test.responseBody),
			}}
			api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, nil)
			api.httpClients = func(proxyURL string) (pluginapi.HostHTTPClient, error) {
				if proxyURL != "" {
					t.Fatalf("proxy URL = %q, want empty", proxyURL)
				}
				return client, nil
			}
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   "/v0/management/plugins/" + provider.ID + "/models",
				Body:   body,
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
			}
			if client.calls != 1 || client.request.Method != http.MethodGet || client.request.URL != "https://draft.example.com/v1/models" {
				t.Fatalf("request = %#v, calls = %d", client.request, client.calls)
			}
			if got := client.request.Headers.Get(test.authHeader); got != test.authValue {
				t.Fatalf("%s = %q, want %q", test.authHeader, got, test.authValue)
			}
			if test.protocol == config.ProtocolAnthropic && client.request.Headers.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatalf("Anthropic-Version = %q", client.request.Headers.Get("Anthropic-Version"))
			}
			var result struct {
				Models []discoveredModel `json:"models"`
			}
			decodeResponse(t, resp, &result)
			if len(result.Models) != 1 || result.Models[0].Name != test.wantName || result.Models[0].DisplayName != test.wantDisplay {
				t.Fatalf("models = %#v", result.Models)
			}
			if bytes.Contains(resp.Body, []byte("sk-discovery")) {
				t.Fatalf("response leaked API key: %s", resp.Body)
			}
		})
	}
}

func TestDiscoverModelsSupportsDirectAndNoneProxyModes(t *testing.T) {
	for _, proxyMode := range []string{"direct", "none"} {
		t.Run(proxyMode, func(t *testing.T) {
			cfg := config.Default()
			cfg.BaseURL = "https://draft.example.com/v1"
			body, _ := json.Marshal(map[string]any{
				"config": cfg,
				"key": map[string]any{
					"id": "draft", "api-key": "sk-private", "proxy-url": proxyMode,
				},
			})
			client := &fakeHTTPClient{response: pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Body:       []byte(`{"data":[]}`),
			}}
			api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, nil)
			api.httpClients = func(proxyURL string) (pluginapi.HostHTTPClient, error) {
				if proxyURL != proxyMode {
					t.Fatalf("proxy URL = %q, want %q", proxyURL, proxyMode)
				}
				return client, nil
			}
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/models", Body: body,
			})
			if resp.StatusCode != http.StatusOK || client.calls != 1 {
				t.Fatalf("status = %d, calls = %d, body = %s", resp.StatusCode, client.calls, resp.Body)
			}
			if bytes.Contains(resp.Body, []byte("sk-private")) {
				t.Fatalf("model response leaked API key: %s", resp.Body)
			}
		})
	}
}

func TestDiscoverModelsRedactsTransportErrors(t *testing.T) {
	t.Parallel()
	const apiKey = "sk-private-discovery"
	const headerSecret = "private-header-value"
	cfg := config.Default()
	cfg.BaseURL = "https://draft.example.com/v1"
	cfg.Headers = map[string]string{"X-Private-Metadata": headerSecret}
	body, _ := json.Marshal(map[string]any{
		"config": cfg,
		"key":    map[string]any{"id": "draft", "api-key": apiKey},
	})
	client := &fakeHTTPClient{err: errors.New("request failed with " + apiKey + " and " + headerSecret)}
	api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, nil)
	api.httpClients = func(string) (pluginapi.HostHTTPClient, error) { return client, nil }
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/models", Body: body,
	})
	if resp.StatusCode != http.StatusBadGateway || !bytes.Contains(resp.Body, []byte("[REDACTED]")) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if bytes.Contains(resp.Body, []byte(apiKey)) || bytes.Contains(resp.Body, []byte(headerSecret)) {
		t.Fatalf("transport error leaked discovery secrets: %s", resp.Body)
	}
}

func TestConnectionResolvesStoredSecretAndRedactsErrors(t *testing.T) {
	t.Parallel()
	const secret = "sk-private-connection-secret"
	tester := &fakeTester{err: errors.New("upstream rejected " + secret)}
	cfg := config.Default()
	cfg.BaseURL = "https://api.example.com/v1"
	cfg.Models = []config.Model{{Name: "model-one"}}
	store := &fakeAuthStore{raw: mustCredentialForConfig(t, cfg, []providerauth.Key{{ID: "primary", APIKey: secret}})}
	body, err := json.Marshal(map[string]any{
		"config": cfg,
		"key":    map[string]any{"id": "primary", "api-key": ""},
		"model":  "model-one",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	api := New(provider.ID, provider.CredentialsFile, cfg, store, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/test",
		Body:   body,
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if tester.request.Key.APIKey != secret {
		t.Fatalf("tester key was not resolved: %#v", tester.request.Key)
	}
	if bytes.Contains(resp.Body, []byte(secret)) || !bytes.Contains(resp.Body, []byte("[REDACTED]")) {
		t.Fatalf("error response did not redact secret: %s", resp.Body)
	}
}

func TestConnectionResolvesAndPassesPerKeyProxyToTester(t *testing.T) {
	t.Parallel()
	const proxyURL = "socks5://proxy-user:proxy-secret@proxy.example:1080"
	tester := &fakeTester{}
	cfg := config.Default()
	cfg.BaseURL = "https://api.example.com/v1"
	cfg.Models = []config.Model{{Name: "model-one"}}
	store := &fakeAuthStore{raw: mustCredentialForConfig(t, cfg, []providerauth.Key{{ID: "primary", APIKey: "sk-private", ProxyURL: proxyURL}})}
	body, _ := json.Marshal(map[string]any{
		"config": cfg,
		"key": map[string]any{
			"id": "primary", "api-key": "", "proxy-url": "socks5://redacted@proxy.example:1080", "preserve-proxy-url": true,
		},
		"model": "model-one",
	})
	api := New(provider.ID, provider.CredentialsFile, cfg, store, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/test", Body: body,
	})
	if resp.StatusCode != http.StatusOK || tester.calls != 1 || tester.request.Key.ProxyURL != proxyURL {
		t.Fatalf("status = %d, tester calls = %d, body = %s", resp.StatusCode, tester.calls, resp.Body)
	}
	if bytes.Contains(resp.Body, []byte("proxy-secret")) || bytes.Contains(resp.Body, []byte("sk-private")) {
		t.Fatalf("connection response leaked credentials: %s", resp.Body)
	}
}

func TestConnectionSupportsDirectAndNoneProxyModes(t *testing.T) {
	for _, proxyMode := range []string{"direct", "none"} {
		t.Run(proxyMode, func(t *testing.T) {
			tester := &fakeTester{}
			cfg := config.Default()
			cfg.BaseURL = "https://api.example.com/v1"
			cfg.Models = []config.Model{{Name: "model-one"}}
			body, _ := json.Marshal(map[string]any{
				"config": cfg,
				"key": map[string]any{
					"id": "draft", "api-key": "sk-private", "proxy-url": proxyMode,
				},
				"model": "model-one",
			})
			api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, tester)
			resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
				Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/test", Body: body,
			})
			if resp.StatusCode != http.StatusOK || tester.calls != 1 || tester.request.Key.ProxyURL != proxyMode {
				t.Fatalf("status = %d, calls = %d, body = %s", resp.StatusCode, tester.calls, resp.Body)
			}
			if bytes.Contains(resp.Body, []byte("sk-private")) {
				t.Fatalf("connection response leaked API key: %s", resp.Body)
			}
		})
	}
}

func TestConnectionReturnsResult(t *testing.T) {
	t.Parallel()
	tester := &fakeTester{result: TestResult{Message: "ok", StatusCode: http.StatusOK, LatencyMS: 12}}
	cfg := config.Default()
	cfg.BaseURL = "https://api.example.com/v1"
	cfg.Models = []config.Model{{Name: "model-one"}}
	body, _ := json.Marshal(map[string]any{
		"config": cfg,
		"key":    map[string]any{"id": "draft", "api-key": "sk-draft-secret"},
		"model":  "model-one",
	})
	api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/test",
		Body:   body,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte(`"latency-ms":12`)) || bytes.Contains(resp.Body, []byte("sk-draft-secret")) {
		t.Fatalf("response = %s", resp.Body)
	}
}

func TestConnectionRejectsImageModelWithoutCallingTester(t *testing.T) {
	t.Parallel()
	tester := &fakeTester{}
	cfg := config.Default()
	cfg.BaseURL = "https://api.example.com/v1"
	cfg.Models = []config.Model{
		{Name: "chat-model"},
		{Name: "image-model", Alias: "image", Image: true},
	}
	body, _ := json.Marshal(map[string]any{
		"config": cfg,
		"key":    map[string]any{"id": "draft", "api-key": "sk-draft-secret"},
		"model":  "image",
	})
	api := New(provider.ID, provider.CredentialsFile, cfg, &fakeAuthStore{}, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/test",
		Body:   body,
	})
	if resp.StatusCode != http.StatusBadRequest || tester.calls != 0 || !bytes.Contains(resp.Body, []byte("image_model_not_testable")) {
		t.Fatalf("status = %d, tester calls = %d, body = %s", resp.StatusCode, tester.calls, resp.Body)
	}
}

func TestConnectionAllowsExplicitSecretForChangedDestination(t *testing.T) {
	t.Parallel()
	stored := config.Default()
	stored.BaseURL = "https://saved.example.com/v1"
	stored.Models = []config.Model{{Name: "model-one"}}
	draft := stored
	draft.BaseURL = "https://draft.example.com/v1"
	tester := &fakeTester{result: TestResult{Message: "ok", StatusCode: http.StatusOK}}
	body, _ := json.Marshal(map[string]any{
		"config": draft,
		"key":    map[string]any{"id": "primary", "api-key": "sk-reentered-secret"},
		"model":  "model-one",
	})
	api := New(provider.ID, provider.CredentialsFile, stored, &fakeAuthStore{}, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + provider.ID + "/test",
		Body:   body,
	})
	if resp.StatusCode != http.StatusOK || tester.calls != 1 || tester.request.Config.BaseURL != draft.BaseURL {
		t.Fatalf("status = %d, tester calls = %d, request = %#v, body = %s", resp.StatusCode, tester.calls, tester.request, resp.Body)
	}
}

func TestConnectionRequiresSecretReentryForChangedDestination(t *testing.T) {
	t.Parallel()
	stored := config.Default()
	stored.BaseURL = "https://saved.example.com/v1"
	stored.Models = []config.Model{{Name: "model-one"}}
	draft := stored
	draft.BaseURL = "https://attacker.example.com/v1"
	store := &fakeAuthStore{raw: mustCredential(t, []providerauth.Key{{ID: "primary", APIKey: "sk-private-secret"}})}
	tester := &fakeTester{}
	body, _ := json.Marshal(map[string]any{
		"config": draft,
		"key":    map[string]any{"id": "primary", "api-key": ""},
		"model":  "model-one",
	})
	api := New(provider.ID, provider.CredentialsFile, stored, store, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/test", Body: body,
	})
	if resp.StatusCode != http.StatusBadRequest || tester.calls != 0 || !bytes.Contains(resp.Body, []byte("credential_reentry_required")) {
		t.Fatalf("status = %d, tester calls = %d, body = %s", resp.StatusCode, tester.calls, resp.Body)
	}
}

func TestConnectionRequiresSecretReentryWhenStoredBindingDiffersFromCurrentConfig(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.BaseURL = "https://saved.example.com/v1"
	cfg.Models = []config.Model{{Name: "model-one"}}
	other := cfg
	other.Protocol = config.ProtocolGemini
	store := &fakeAuthStore{raw: mustCredentialForConfig(t, other, []providerauth.Key{{ID: "primary", APIKey: "sk-private-secret"}})}
	tester := &fakeTester{}
	body, _ := json.Marshal(map[string]any{
		"config": cfg,
		"key":    map[string]any{"id": "primary", "api-key": ""},
		"model":  "model-one",
	})
	api := New(provider.ID, provider.CredentialsFile, cfg, store, tester)
	resp, _ := api.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: "/v0/management/plugins/" + provider.ID + "/test", Body: body,
	})
	if resp.StatusCode != http.StatusBadRequest || tester.calls != 0 || !bytes.Contains(resp.Body, []byte("credential_reentry_required")) {
		t.Fatalf("status = %d, tester calls = %d, body = %s", resp.StatusCode, tester.calls, resp.Body)
	}
}

func mustCredential(t *testing.T, keys []providerauth.Key) []byte {
	t.Helper()
	return mustCredentialForConfig(t, disabledConfig(), keys)
}

func mustCredentialForConfig(t *testing.T, cfg config.Config, keys []providerauth.Key) []byte {
	t.Helper()
	destination := providerauth.DestinationForConfig(cfg)
	raw, err := providerauth.MarshalCredentialFile(providerauth.CredentialFile{Type: provider.ID, Destination: &destination, Keys: keys})
	if err != nil {
		t.Fatalf("MarshalCredentialFile() error = %v", err)
	}
	return raw
}

func decodeResponse(t *testing.T, resp pluginapi.ManagementResponse, target any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body, target); err != nil {
		t.Fatalf("decode response %q: %v", resp.Body, err)
	}
}

func stringsContain(value, fragment string) bool {
	return bytes.Contains([]byte(value), []byte(fragment))
}
