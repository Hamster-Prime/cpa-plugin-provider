package config

import (
	"reflect"
	"testing"
)

func TestParseNormalizesProviderConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
name: " Example "
protocol: anthropic-messages
base-url: https://api.example.com/v1/
prefix: team/
models:
  - name: claude-upstream
    alias: claude
    input-modalities: [Text, image, text]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Name != "Example" || cfg.BaseURL != "https://api.example.com/v1" || cfg.Prefix != "team" {
		t.Fatalf("normalized config = %#v", cfg)
	}
	if got := cfg.ModelID(cfg.Models[0]); got != "team/claude" {
		t.Fatalf("ModelID() = %q", got)
	}
	if got := cfg.BaseModelID(cfg.Models[0]); got != "claude" {
		t.Fatalf("BaseModelID() = %q", got)
	}
	if !reflect.DeepEqual(cfg.Models[0].InputModalities, []string{"text", "image"}) {
		t.Fatalf("input modalities = %#v", cfg.Models[0].InputModalities)
	}
}

func TestProtocolExecutorFormats(t *testing.T) {
	want := map[Protocol]string{
		ProtocolOpenAIChat:      "openai",
		ProtocolOpenAIResponses: "openai-response",
		ProtocolAnthropic:       "claude",
		ProtocolGemini:          "gemini",
	}
	for protocol, expected := range want {
		if got := protocol.ExecutorFormat(); got != expected {
			t.Errorf("%s.ExecutorFormat() = %q, want %q", protocol, got, expected)
		}
	}
}

func TestPluginModelsMirrorOpenAICompatibilityAdvancedFields(t *testing.T) {
	cfg := Config{
		Name:     "Example",
		Protocol: ProtocolGemini,
		Prefix:   "edge",
		Models: []Model{
			{Name: "image-upstream", Alias: "image", Image: true},
			{Name: "reasoning-upstream", Alias: "reasoning", Thinking: &ThinkingSupport{Min: 128, Max: 32768, ZeroAllowed: true, DynamicAllowed: true}},
			{Name: "default-thinking"},
		},
	}
	models := cfg.PluginModels()
	if len(models) != 3 {
		t.Fatalf("PluginModels() len = %d", len(models))
	}
	if models[0].ID != "image" || models[0].Type != "openai-image" || models[0].Thinking != nil {
		t.Fatalf("image model = %#v", models[0])
	}
	if models[1].Thinking == nil || models[1].Thinking.Max != 32768 || !models[1].Thinking.ZeroAllowed {
		t.Fatalf("configured thinking = %#v", models[1].Thinking)
	}
	if models[1].UserDefined {
		t.Fatal("configured model bypasses CPA thinking validation")
	}
	if models[2].Thinking == nil || !reflect.DeepEqual(models[2].Thinking.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("default thinking = %#v", models[2].Thinking)
	}
}

func TestParseRejectsInvalidURLAndDuplicateAliases(t *testing.T) {
	for _, raw := range []string{
		"base-url: ftp://example.com\n",
		"base-url: https://example.com/v1?tenant=one\n",
		"base-url: https://example.com/v1#section\n",
		"models:\n  - name: model-without-base-url\n",
		"models:\n  - name: one\n    alias: same\n  - name: two\n    alias: same\n",
		"base-url: https://example.com/v1\nmodels:\n  - name: public-b\n    alias: public-a\n  - name: upstream-b\n    alias: public-b\n",
		"base-url: https://example.com/v1\nprefix: team\nmodels:\n  - name: upstream-a\n    alias: public-a\n  - name: team/public-a\n    alias: public-b\n",
		"base-url: https://example.com/v1\nprefix: p\nmodels:\n  - name: upstream-a\n    alias: p/foo\n  - name: upstream-b\n    alias: foo\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestResolveModelUsesUnambiguousMatchPriority(t *testing.T) {
	cfg := Config{Prefix: "team", Models: []Model{
		{Name: "vendor-model(v2)"},
		{Name: "public-b", Alias: "public-a"},
		{Name: "upstream-b", Alias: "public-b"},
		{Name: "thinking-upstream", Alias: "thinking"},
	}}
	for _, tt := range []struct {
		name      string
		requested string
		wantName  string
		wantOK    bool
	}{
		{name: "exact parenthesized model", requested: "vendor-model(v2)", wantName: "vendor-model(v2)", wantOK: true},
		{name: "public alias before earlier upstream name", requested: "public-b", wantName: "upstream-b", wantOK: true},
		{name: "valid prefixed thinking suffix", requested: "team/thinking(high)", wantName: "thinking-upstream", wantOK: true},
		{name: "valid numeric thinking suffix", requested: "thinking(8192)", wantName: "thinking-upstream", wantOK: true},
		{name: "unknown parenthesized suffix", requested: "thinking(v2)", wantOK: false},
		{name: "space before suffix is part of model id", requested: "thinking (high)", wantOK: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, ok := cfg.ResolveModel(tt.requested)
			if ok != tt.wantOK || model.Name != tt.wantName {
				t.Fatalf("ResolveModel(%q) = %#v, %v; want name %q, ok %v", tt.requested, model, ok, tt.wantName, tt.wantOK)
			}
		})
	}
}
