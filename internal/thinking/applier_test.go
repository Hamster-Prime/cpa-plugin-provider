package thinking

import (
	"context"
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestApplyThinkingUsesProtocolSpecificFields(t *testing.T) {
	tests := []struct {
		name     string
		protocol config.Protocol
		config   pluginapi.ThinkingConfig
		body     string
		path     string
		want     any
	}{
		{"chat budget", config.ProtocolOpenAIChat, pluginapi.ThinkingConfig{Mode: "budget", Budget: 9000}, `{}`, "reasoning_effort", "high"},
		{"chat none fallback", config.ProtocolOpenAIChat, pluginapi.ThinkingConfig{Mode: "none", Level: "low"}, `{}`, "reasoning_effort", "low"},
		{"responses level", config.ProtocolOpenAIResponses, pluginapi.ThinkingConfig{Mode: "level", Level: "HIGH"}, `{}`, "reasoning.effort", "high"},
		{"gemini budget", config.ProtocolGemini, pluginapi.ThinkingConfig{Mode: "budget", Budget: 8192}, `{}`, "generationConfig.thinkingConfig.thinkingBudget", int64(8192)},
		{"gemini auto", config.ProtocolGemini, pluginapi.ThinkingConfig{Mode: "auto"}, `{}`, "generationConfig.thinkingConfig.thinkingBudget", int64(-1)},
		{"claude level", config.ProtocolAnthropic, pluginapi.ThinkingConfig{Mode: "level", Level: "high"}, `{}`, "output_config.effort", "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := NewApplier(config.Config{Protocol: tt.protocol}).ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
				Config: tt.config,
				Body:   []byte(tt.body),
			})
			if err != nil {
				t.Fatal(err)
			}
			value := gjson.GetBytes(resp.Body, tt.path)
			switch want := tt.want.(type) {
			case string:
				if value.String() != want {
					t.Fatalf("%s = %q, want %q; body=%s", tt.path, value.String(), want, resp.Body)
				}
			case int64:
				if value.Int() != want {
					t.Fatalf("%s = %d, want %d; body=%s", tt.path, value.Int(), want, resp.Body)
				}
			}
		})
	}
}

func TestClaudeBudgetIsClampedToModelAndMaxTokens(t *testing.T) {
	resp, err := NewApplier(config.Config{Protocol: config.ProtocolAnthropic}).ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Config: pluginapi.ThinkingConfig{Mode: "budget", Budget: 20000},
		Model:  pluginapi.ModelInfo{Thinking: &pluginapi.ThinkingSupport{Min: 1024, Max: 16000}},
		Body:   []byte(`{"max_tokens":4096,"output_config":{"effort":"low"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(resp.Body, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q; body=%s", got, resp.Body)
	}
	if got := gjson.GetBytes(resp.Body, "thinking.budget_tokens").Int(); got != 4095 {
		t.Fatalf("thinking.budget_tokens = %d; body=%s", got, resp.Body)
	}
	if gjson.GetBytes(resp.Body, "output_config").Exists() {
		t.Fatalf("empty output_config was retained: %s", resp.Body)
	}
}

func TestClaudeDisablesThinkingWhenMaxTokensCannotFitMinimum(t *testing.T) {
	resp, err := NewApplier(config.Config{Protocol: config.ProtocolAnthropic}).ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Config: pluginapi.ThinkingConfig{Mode: "budget", Budget: 20000},
		Model:  pluginapi.ModelInfo{Thinking: &pluginapi.ThinkingSupport{Min: 1024, Max: 16000}},
		Body:   []byte(`{"max_tokens":1024}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(resp.Body, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, resp.Body)
	}
	if gjson.GetBytes(resp.Body, "thinking.budget_tokens").Exists() {
		t.Fatalf("infeasible thinking budget was retained: %s", resp.Body)
	}
}

func TestGeminiPreservesExplicitIncludeThoughtsAndCanDisable(t *testing.T) {
	a := NewApplier(config.Config{Protocol: config.ProtocolGemini})
	resp, err := a.ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Config: pluginapi.ThinkingConfig{Mode: "level", Level: "medium"},
		Body:   []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":false,"thinkingBudget":100}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(resp.Body, "generationConfig.thinkingConfig.thinkingLevel").String(); got != "medium" {
		t.Fatalf("thinkingLevel = %q; body=%s", got, resp.Body)
	}
	if got := gjson.GetBytes(resp.Body, "generationConfig.thinkingConfig.includeThoughts").Bool(); got {
		t.Fatalf("includeThoughts was not preserved: %s", resp.Body)
	}
	if gjson.GetBytes(resp.Body, "generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("conflicting budget retained: %s", resp.Body)
	}

	disabled, err := a.ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Config: pluginapi.ThinkingConfig{Mode: "none"},
		Body:   resp.Body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(disabled.Body, "generationConfig.thinkingConfig").Exists() {
		t.Fatalf("thinkingConfig was not removed: %s", disabled.Body)
	}
}

func TestInvalidBodyBecomesValidJSONAndIdentifierMatchesProvider(t *testing.T) {
	a := NewApplier(config.Config{Protocol: config.ProtocolOpenAIResponses})
	if a.Identifier() != provider.ID {
		t.Fatalf("Identifier() = %q", a.Identifier())
	}
	resp, err := a.ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Config: pluginapi.ThinkingConfig{Mode: "none"},
		Body:   []byte(`not-json`),
	})
	if err != nil || !gjson.ValidBytes(resp.Body) || gjson.GetBytes(resp.Body, "reasoning.effort").String() != "none" {
		t.Fatalf("ApplyThinking() body=%s error=%v", resp.Body, err)
	}
}
