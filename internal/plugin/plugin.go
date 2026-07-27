package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	providerauth "github.com/Hamster-Prime/cpa-plugin-provider/internal/auth"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/executor"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/management"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/models"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	thinkingpkg "github.com/Hamster-Prime/cpa-plugin-provider/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Dependencies struct {
	AuthStore management.HostAuthStore
}

type ProviderPlugin struct {
	config     config.Config
	auth       *providerauth.Provider
	models     *models.Provider
	thinking   *thinkingpkg.Applier
	executor   *executor.Executor
	management *management.API
}

func Build(configYAML []byte, dependencies Dependencies) (pluginapi.Plugin, *ProviderPlugin, error) {
	cfg, err := config.Parse(configYAML)
	if err != nil {
		return pluginapi.Plugin{}, nil, err
	}
	p := &ProviderPlugin{
		config:   cfg,
		auth:     providerauth.NewProvider(cfg),
		models:   models.NewProvider(cfg),
		thinking: thinkingpkg.NewApplier(cfg),
		executor: executor.New(cfg),
	}
	p.management = management.New(provider.ID, provider.CredentialsFile, cfg, dependencies.AuthStore, p)

	formats := []string{cfg.Protocol.ExecutorFormat(), "openai-image"}
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             provider.DisplayName,
			Version:          provider.Version,
			Author:           provider.Author,
			GitHubRepository: provider.Repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "name", Type: pluginapi.ConfigFieldTypeString, Description: "Provider display name."},
				{Name: "protocol", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: config.ProtocolValues(), Description: "Upstream request protocol."},
				{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "Versioned upstream API base URL."},
				{Name: "prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Optional public model namespace."},
				{Name: "disabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Disable provider routing."},
				{Name: "disable-cooling", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Disable CPA cooldown for provider keys."},
				{Name: "headers", Type: pluginapi.ConfigFieldTypeObject, Description: "Additional upstream HTTP headers."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Custom model definitions."},
				{Name: "test-model", Type: pluginapi.ConfigFieldTypeString, Description: "Default model used by connection tests."},
			},
		},
		Capabilities: pluginapi.Capabilities{
			AuthProvider:  p,
			ModelProvider: p,
			Executor:      p,
			// Auth-bound registration lets CPA apply each credential's model prefix.
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: append([]string(nil), formats...),
			ThinkingApplier:       p,
			ManagementAPI:         p,
		},
	}, p, nil
}

func (p *ProviderPlugin) Identifier() string { return provider.ID }

func (p *ProviderPlugin) ParseAuth(ctx context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	return p.auth.ParseAuth(ctx, req)
}

func (p *ProviderPlugin) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return p.auth.StartLogin(ctx, req)
}

func (p *ProviderPlugin) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return p.auth.PollLogin(ctx, req)
}

func (p *ProviderPlugin) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	return p.auth.RefreshAuth(ctx, req)
}

func (p *ProviderPlugin) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.StaticModels(ctx, req)
}

func (p *ProviderPlugin) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	response := pluginapi.ModelResponse{Provider: provider.ID}
	if providerauth.StorageMatchesConfig(req.StorageJSON, p.config) {
		var err error
		response, err = p.models.ModelsForAuth(ctx, req)
		if err != nil {
			return pluginapi.ModelResponse{}, err
		}
	}
	refreshed, err := p.auth.RefreshAuth(ctx, pluginapi.AuthRefreshRequest{
		AuthID:       req.AuthID,
		AuthProvider: req.AuthProvider,
		StorageJSON:  req.StorageJSON,
		Metadata:     req.Metadata,
		Attributes:   req.Attributes,
		Host:         req.Host,
		HTTPClient:   req.HTTPClient,
	})
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	response.AuthUpdate = refreshed.Auth
	return response, nil
}

func (p *ProviderPlugin) ApplyThinking(ctx context.Context, req pluginapi.ThinkingApplyRequest) (pluginapi.PayloadResponse, error) {
	return p.thinking.ApplyThinking(ctx, req)
}

func (p *ProviderPlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.Execute(ctx, req)
}

func (p *ProviderPlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return p.executor.ExecuteStream(ctx, req)
}

func (p *ProviderPlugin) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.CountTokens(ctx, req)
}

func (p *ProviderPlugin) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return p.executor.HttpRequest(ctx, req)
}

func (p *ProviderPlugin) RegisterManagement(ctx context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return p.management.RegisterManagement(ctx, req)
}

func (p *ProviderPlugin) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return p.management.HandleManagement(ctx, req)
}

func (p *ProviderPlugin) TestConnection(ctx context.Context, req management.TestRequest) (management.TestResult, error) {
	if req.HTTPClient == nil {
		return management.TestResult{}, fmt.Errorf("host HTTP client is unavailable")
	}
	if strings.TrimSpace(req.Key.ProxyURL) != "" {
		return management.TestResult{}, fmt.Errorf("connection testing with a per-key proxy is unsupported")
	}
	storage, err := json.Marshal(map[string]any{
		"type":        provider.ID,
		"destination": providerauth.DestinationForConfig(req.Config),
		"id":          req.Key.ID,
		"api-key":     req.Key.APIKey,
	})
	if err != nil {
		return management.TestResult{}, err
	}
	payload, err := testPayload(req.Config.Protocol, req.Model)
	if err != nil {
		return management.TestResult{}, err
	}
	probe := executor.New(req.Config)
	_, err = probe.Execute(ctx, pluginapi.ExecutorRequest{
		AuthProvider: provider.ID,
		Model:        req.Model,
		Format:       req.Config.Protocol.ExecutorFormat(),
		SourceFormat: req.Config.Protocol.ExecutorFormat(),
		Headers:      http.Header{"Content-Type": []string{"application/json"}},
		Payload:      payload,
		StorageJSON:  storage,
		HTTPClient:   req.HTTPClient,
	})
	if err != nil {
		return management.TestResult{}, err
	}
	return management.TestResult{Message: "连接成功", StatusCode: http.StatusOK, Model: req.Model}, nil
}

func testPayload(protocol config.Protocol, model string) ([]byte, error) {
	var payload any
	switch protocol {
	case config.ProtocolOpenAIResponses:
		payload = map[string]any{"model": model, "input": "OK", "max_output_tokens": 1}
	case config.ProtocolAnthropic:
		payload = map[string]any{
			"model": model, "max_tokens": 1,
			"messages": []any{map[string]any{"role": "user", "content": "OK"}},
		}
	case config.ProtocolGemini:
		payload = map[string]any{
			"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "OK"}}}},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		}
	default:
		payload = map[string]any{
			"model": model, "max_tokens": 1,
			"messages": []any{map[string]any{"role": "user", "content": "OK"}},
		}
	}
	return json.Marshal(payload)
}

var _ pluginapi.AuthProvider = (*ProviderPlugin)(nil)
var _ pluginapi.ModelProvider = (*ProviderPlugin)(nil)
var _ pluginapi.ProviderExecutor = (*ProviderPlugin)(nil)
var _ pluginapi.ThinkingApplier = (*ProviderPlugin)(nil)
var _ pluginapi.ManagementAPI = (*ProviderPlugin)(nil)
var _ pluginapi.ManagementHandler = (*ProviderPlugin)(nil)
var _ management.ConnectionTester = (*ProviderPlugin)(nil)
