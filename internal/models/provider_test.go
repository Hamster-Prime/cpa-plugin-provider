package models

import (
	"context"
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	providerinfo "github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProviderReturnsConfiguredStaticModels(t *testing.T) {
	p := NewProvider(config.Config{
		Name:     "Acme",
		Protocol: config.ProtocolAnthropic,
		Prefix:   "team",
		Models: []config.Model{
			{Name: "upstream-chat", Alias: "chat", InputModalities: []string{"text"}},
			{Name: "upstream-image", Alias: "image", Image: true},
		},
	})
	resp, err := p.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Provider != providerinfo.ID || len(resp.Models) != 2 {
		t.Fatalf("StaticModels() = %#v", resp)
	}
	if resp.Models[0].ID != "chat" || resp.Models[0].Thinking == nil || resp.Models[0].UserDefined {
		t.Fatalf("chat model = %#v", resp.Models[0])
	}
	if resp.Models[1].Type != "openai-image" || resp.Models[1].Thinking != nil {
		t.Fatalf("image model = %#v", resp.Models[1])
	}

	resp.Models[0].ID = "mutated"
	again, err := p.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{})
	if err != nil || again.Models[0].ID != "chat" {
		t.Fatalf("ModelsForAuth() = %#v, %v", again, err)
	}
}

func TestDisabledProviderReturnsNoModels(t *testing.T) {
	p := NewProvider(config.Config{Disabled: true, Models: []config.Model{{Name: "model"}}})
	resp, err := p.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil || len(resp.Models) != 0 {
		t.Fatalf("StaticModels() = %#v, %v", resp, err)
	}
}
