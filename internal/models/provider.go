package models

import (
	"context"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Provider struct {
	config config.Config
}

var _ pluginapi.ModelProvider = (*Provider)(nil)

func NewProvider(cfg config.Config) *Provider {
	cfg = provider.CloneConfig(cfg)
	cfg.Normalize()
	return &Provider{config: cfg}
}

func (p *Provider) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider.ID, Models: p.config.PluginModels()}, nil
}

func (p *Provider) ModelsForAuth(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider.ID, Models: p.config.PluginModels()}, nil
}
