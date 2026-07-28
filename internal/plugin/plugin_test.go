package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/management"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildDeclaresSelectedProtocolAndImage(t *testing.T) {
	registered, instance, err := Build([]byte(`
name: Test
protocol: anthropic-messages
base-url: https://api.example.com/v1
models:
  - name: upstream
    alias: public
`), Dependencies{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if instance == nil || registered.Capabilities.AuthProvider != instance || registered.Capabilities.ManagementAPI != instance {
		t.Fatalf("capabilities are not wired to plugin instance")
	}
	wantInput := []string{"claude", "openai-image"}
	wantOutput := []string{"openai", "openai-response", "claude", "gemini", "openai-image"}
	if !equalStrings(registered.Capabilities.ExecutorInputFormats, wantInput) || !equalStrings(registered.Capabilities.ExecutorOutputFormats, wantOutput) {
		t.Fatalf("executor formats = %#v / %#v, want %#v / %#v", registered.Capabilities.ExecutorInputFormats, registered.Capabilities.ExecutorOutputFormats, wantInput, wantOutput)
	}
	if registered.Metadata.GitHubRepository != provider.Repository || len(registered.Metadata.ConfigFields) == 0 {
		t.Fatalf("metadata = %#v", registered.Metadata)
	}
	if registered.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor model scope = %q, want auth-bound scope", registered.Capabilities.ExecutorModelScope)
	}
}

func TestBuildDeclaresCodexInputForResponsesUpstream(t *testing.T) {
	registered, _, err := Build([]byte(`
protocol: openai-responses
base-url: https://api.example.com/v1
models:
  - name: gpt-test
`), Dependencies{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := []string{"codex", "openai-image"}
	if !equalStrings(registered.Capabilities.ExecutorInputFormats, want) {
		t.Fatalf("executor input formats = %#v, want %#v", registered.Capabilities.ExecutorInputFormats, want)
	}
}

func TestBuildRejectsInvalidConfiguredProvider(t *testing.T) {
	if _, _, err := Build([]byte("models:\n  - name: missing-url\n"), Dependencies{}); err == nil {
		t.Fatal("Build() unexpectedly accepted model configuration without base URL")
	}
}

func TestModelsForAuthReturnsCurrentRoutingAuthUpdate(t *testing.T) {
	cfg := config.Config{
		Name: "Updated provider", Priority: 7, Prefix: "current", DisableCooling: true,
		Protocol: config.ProtocolOpenAIChat, BaseURL: "https://api.example.com/v1",
		Models: []config.Model{{Name: "upstream", Alias: "public"}},
	}
	_, instance, err := Build(mustYAML(t, cfg), Dependencies{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	storage, err := json.Marshal(map[string]any{
		"type":        provider.ID,
		"destination": providerauth.DestinationForConfig(cfg),
		"id":          "primary",
		"api-key":     "secret",
		"proxy-url":   "direct",
		"priority":    3,
	})
	if err != nil {
		t.Fatalf("marshal auth storage: %v", err)
	}
	response, err := instance.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{
		AuthID:       "credentials.json#primary",
		AuthProvider: provider.ID,
		StorageJSON:  storage,
		Metadata:     map[string]any{"disable_cooling": false, "priority": -100},
		Attributes:   map[string]string{"runtime_only": "true", "priority": "-100"},
	})
	if err != nil {
		t.Fatalf("ModelsForAuth() error = %v", err)
	}
	update := response.AuthUpdate
	if len(response.Models) != 1 || update.ID != "credentials.json#primary" || update.Prefix != "current" || update.ProxyURL != "direct" || update.Disabled {
		t.Fatalf("ModelsForAuth() = %#v", response)
	}
	if update.Label != "Updated provider" || update.Metadata["priority"] != 10 || update.Metadata["disable_cooling"] != true {
		t.Fatalf("current metadata/label = %#v / %q", update.Metadata, update.Label)
	}
	if update.Attributes["priority"] != "10" || update.Attributes["runtime_only"] != "true" {
		t.Fatalf("current attributes = %#v", update.Attributes)
	}
}

func TestModelsForAuthHidesModelsForUnboundOrMismatchedCredential(t *testing.T) {
	cfg := config.Config{
		Name: "Bound provider", Protocol: config.ProtocolOpenAIChat, BaseURL: "https://api.example.com/v1",
		Models: []config.Model{{Name: "upstream", Alias: "public"}},
	}
	_, instance, err := Build(mustYAML(t, cfg), Dependencies{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	other := cfg
	other.Protocol = config.ProtocolAnthropic
	mismatched, err := json.Marshal(map[string]any{
		"type": provider.ID, "destination": providerauth.DestinationForConfig(other), "id": "primary", "api-key": "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		storage []byte
	}{
		{name: "legacy unbound", storage: []byte(`{"type":"multi-protocol-provider","id":"primary","api-key":"secret"}`)},
		{name: "protocol mismatch", storage: mismatched},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, errModels := instance.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{
				AuthID: "credentials.json#primary", AuthProvider: provider.ID, StorageJSON: test.storage,
			})
			if errModels != nil {
				t.Fatalf("ModelsForAuth() error = %v", errModels)
			}
			if len(response.Models) != 0 || response.AuthUpdate.ID != "credentials.json#primary" {
				t.Fatalf("ModelsForAuth() = %#v, want auth update without models", response)
			}
		})
	}
}

func TestConnectionUsesProtocolPayloadAndResolvedUpstreamModel(t *testing.T) {
	var requestPath string
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		var err error
		requestBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer server.Close()

	cfg := config.Config{
		Name: "Test", Protocol: config.ProtocolGemini, BaseURL: server.URL + "/v1beta",
		Models: []config.Model{{Name: "native-model", Alias: "public-model"}},
	}
	_, instance, err := Build(mustYAML(t, cfg), Dependencies{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	result, err := instance.TestConnection(context.Background(), management.TestRequest{
		Config: cfg,
		Key:    providerauth.Key{APIKey: "secret", ProxyURL: "direct"},
		Model:  "public-model",
	})
	if err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
	if result.StatusCode != http.StatusOK || requestPath != "/v1beta/models/native-model:generateContent" {
		t.Fatalf("result/path = %#v / %q", result, requestPath)
	}
	var body map[string]any
	if err = json.Unmarshal(requestBody, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if _, exists := body["model"]; exists {
		t.Fatalf("Gemini request body unexpectedly contains model: %#v", body)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mustYAML(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
