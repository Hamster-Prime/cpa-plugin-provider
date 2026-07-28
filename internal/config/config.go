package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type Protocol string

const (
	ProtocolOpenAIChat      Protocol = "openai-chat-completions"
	ProtocolOpenAIResponses Protocol = "openai-responses"
	ProtocolAnthropic       Protocol = "anthropic-messages"
	ProtocolGemini          Protocol = "gemini"
)

const DefaultName = "Custom Provider"

var protocols = []Protocol{
	ProtocolOpenAIChat,
	ProtocolOpenAIResponses,
	ProtocolAnthropic,
	ProtocolGemini,
}

type ThinkingSupport struct {
	Min            int      `yaml:"min,omitempty" json:"min,omitempty"`
	Max            int      `yaml:"max,omitempty" json:"max,omitempty"`
	ZeroAllowed    bool     `yaml:"zero-allowed,omitempty" json:"zero-allowed,omitempty"`
	DynamicAllowed bool     `yaml:"dynamic-allowed,omitempty" json:"dynamic-allowed,omitempty"`
	Levels         []string `yaml:"levels,omitempty" json:"levels,omitempty"`
}

type Model struct {
	Name             string           `yaml:"name" json:"name"`
	Alias            string           `yaml:"alias,omitempty" json:"alias,omitempty"`
	DisplayName      string           `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	ForceMapping     bool             `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
	Image            bool             `yaml:"image,omitempty" json:"image,omitempty"`
	InputModalities  []string         `yaml:"input-modalities,omitempty" json:"input-modalities,omitempty"`
	OutputModalities []string         `yaml:"output-modalities,omitempty" json:"output-modalities,omitempty"`
	Thinking         *ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

type Config struct {
	Name           string            `yaml:"name" json:"name"`
	Priority       int               `yaml:"priority,omitempty" json:"priority"`
	Protocol       Protocol          `yaml:"protocol" json:"protocol"`
	BaseURL        string            `yaml:"base-url" json:"base-url"`
	Prefix         string            `yaml:"prefix,omitempty" json:"prefix"`
	Disabled       bool              `yaml:"disabled,omitempty" json:"disabled"`
	DisableCooling bool              `yaml:"disable-cooling,omitempty" json:"disable-cooling"`
	Headers        map[string]string `yaml:"headers,omitempty" json:"headers"`
	Models         []Model           `yaml:"models,omitempty" json:"models"`
	TestModel      string            `yaml:"test-model,omitempty" json:"test-model"`
}

func Default() Config {
	return Config{
		Name:     DefaultName,
		Protocol: ProtocolOpenAIChat,
		Headers:  map[string]string{},
		Models:   []Model{},
	}
}

func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode plugin config: %w", err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = DefaultName
	}
	c.Protocol = Protocol(strings.ToLower(strings.TrimSpace(string(c.Protocol))))
	if c.Protocol == "" {
		c.Protocol = ProtocolOpenAIChat
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Prefix = strings.Trim(strings.TrimSpace(c.Prefix), "/")
	c.TestModel = strings.TrimSpace(c.TestModel)
	c.Headers = normalizeHeaders(c.Headers)
	c.Models = normalizeModels(c.Models)
}

func (c Config) Validate() error {
	if !c.Protocol.Valid() {
		return fmt.Errorf("unsupported protocol %q", c.Protocol)
	}
	if c.BaseURL != "" {
		parsed, err := url.Parse(c.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("base-url must be an absolute HTTP(S) URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("base-url must use http or https")
		}
		if parsed.User != nil {
			return fmt.Errorf("base-url must not contain credentials")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("base-url must not contain a query or fragment")
		}
	}
	if c.BaseURL == "" && len(c.Models) > 0 {
		return fmt.Errorf("base-url is required when models are configured")
	}
	publicIDOwners := make(map[string]int, len(c.Models)*2)
	for index, model := range c.Models {
		if model.Name == "" {
			return fmt.Errorf("models[%d].name is required", index)
		}
		publicIDs := []string{c.BaseModelID(model)}
		if prefixed := c.ModelID(model); prefixed != publicIDs[0] {
			publicIDs = append(publicIDs, prefixed)
		}
		for _, publicID := range publicIDs {
			if owner, exists := publicIDOwners[publicID]; exists && owner != index {
				return fmt.Errorf("model id %q is published by models[%d] and models[%d]", publicID, owner, index)
			}
			publicIDOwners[publicID] = index
		}
		if model.Thinking != nil {
			if model.Thinking.Min < 0 || model.Thinking.Max < 0 {
				return fmt.Errorf("models[%d].thinking min/max must be non-negative", index)
			}
			if model.Thinking.Max > 0 && model.Thinking.Min > model.Thinking.Max {
				return fmt.Errorf("models[%d].thinking min must not exceed max", index)
			}
		}
	}
	for publicIndex, publicModel := range c.Models {
		publicIDs := []string{c.BaseModelID(publicModel)}
		if prefixed := c.ModelID(publicModel); prefixed != publicIDs[0] {
			publicIDs = append(publicIDs, prefixed)
		}
		for upstreamIndex, upstreamModel := range c.Models {
			if publicIndex == upstreamIndex {
				continue
			}
			for _, publicID := range publicIDs {
				if publicID == upstreamModel.Name {
					return fmt.Errorf("model id %q conflicts with models[%d].name", publicID, upstreamIndex)
				}
			}
		}
	}
	return nil
}

func (p Protocol) Valid() bool {
	for _, candidate := range protocols {
		if p == candidate {
			return true
		}
	}
	return false
}

func (p Protocol) ExecutorFormat() string {
	switch p {
	case ProtocolOpenAIResponses:
		// CPA uses "codex" as the canonical upstream wire format for the
		// OpenAI Responses API. "openai-response" is the client-facing format.
		return "codex"
	case ProtocolAnthropic:
		return "claude"
	case ProtocolGemini:
		return "gemini"
	default:
		return "openai"
	}
}

func (p Protocol) Label() string {
	switch p {
	case ProtocolOpenAIResponses:
		return "OpenAI Responses"
	case ProtocolAnthropic:
		return "Anthropic Messages"
	case ProtocolGemini:
		return "Gemini"
	default:
		return "OpenAI Chat Completions"
	}
}

func ProtocolValues() []string {
	out := make([]string, 0, len(protocols))
	for _, protocol := range protocols {
		out = append(out, string(protocol))
	}
	return out
}

func (c Config) BaseModelID(model Model) string {
	id := strings.TrimSpace(model.Alias)
	if id == "" {
		id = strings.TrimSpace(model.Name)
	}
	return id
}

func (c Config) ModelID(model Model) string {
	id := c.BaseModelID(model)
	if c.Prefix != "" {
		return c.Prefix + "/" + id
	}
	return id
}

func (c Config) ResolveModel(requested string) (Model, bool) {
	requested = strings.TrimSpace(requested)
	if model, found := c.resolveExactPublicModel(requested); found {
		return model, true
	}
	if model, found := c.resolveExactUpstreamModel(requested); found {
		return model, true
	}
	base, hasThinkingSuffix := stripThinkingSuffix(requested)
	if !hasThinkingSuffix {
		return Model{}, false
	}
	if model, found := c.resolveExactPublicModel(base); found {
		return model, true
	}
	return c.resolveExactUpstreamModel(base)
}

func (c Config) resolveExactPublicModel(requested string) (Model, bool) {
	for _, model := range c.Models {
		if requested == c.ModelID(model) || requested == c.BaseModelID(model) {
			return model, true
		}
	}
	return Model{}, false
}

func (c Config) resolveExactUpstreamModel(requested string) (Model, bool) {
	for _, model := range c.Models {
		if requested == model.Name {
			return model, true
		}
	}
	return Model{}, false
}

func (c Config) PluginModels() []pluginapi.ModelInfo {
	if c.Disabled {
		return nil
	}
	out := make([]pluginapi.ModelInfo, 0, len(c.Models))
	for _, model := range c.Models {
		out = append(out, c.PluginModel(model))
	}
	return out
}

func (c Config) PluginModel(model Model) pluginapi.ModelInfo {
	thinking := model.Thinking
	if thinking == nil && !model.Image {
		thinking = &ThinkingSupport{Levels: []string{"low", "medium", "high"}}
	}
	var pluginThinking *pluginapi.ThinkingSupport
	if thinking != nil {
		pluginThinking = &pluginapi.ThinkingSupport{
			Min:            thinking.Min,
			Max:            thinking.Max,
			ZeroAllowed:    thinking.ZeroAllowed,
			DynamicAllowed: thinking.DynamicAllowed,
			Levels:         append([]string(nil), thinking.Levels...),
		}
	}
	modelType := "multi-protocol-provider"
	if model.Image {
		modelType = "openai-image"
	}
	baseID := c.BaseModelID(model)
	displayName := model.DisplayName
	if displayName == "" {
		displayName = baseID
	}
	return pluginapi.ModelInfo{
		// AuthData.Prefix is applied by CPA when these models are registered.
		ID:                        baseID,
		Object:                    "model",
		OwnedBy:                   c.Name,
		Type:                      modelType,
		DisplayName:               displayName,
		Name:                      model.Name,
		Description:               c.Protocol.Label(),
		SupportedInputModalities:  append([]string(nil), model.InputModalities...),
		SupportedOutputModalities: append([]string(nil), model.OutputModalities...),
		Thinking:                  pluginThinking,
		// Match CPA's native OpenAI Compatibility models so the host validates
		// configured thinking limits before invoking the plugin applier.
		UserDefined: false,
	}
}

func normalizeHeaders(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(input))
	for _, key := range keys {
		cleanKey := strings.TrimSpace(key)
		if cleanKey == "" {
			continue
		}
		out[cleanKey] = strings.TrimSpace(input[key])
	}
	return out
}

func normalizeModels(input []Model) []Model {
	out := make([]Model, 0, len(input))
	for _, model := range input {
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		model.DisplayName = strings.TrimSpace(model.DisplayName)
		model.InputModalities = normalizeStringList(model.InputModalities)
		model.OutputModalities = normalizeStringList(model.OutputModalities)
		if model.Thinking != nil {
			model.Thinking.Levels = normalizeStringList(model.Thinking.Levels)
		}
		out = append(out, model)
	}
	return out
}

func normalizeStringList(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, item := range input {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func stripThinkingSuffix(model string) (string, bool) {
	open := strings.LastIndex(model, "(")
	if open <= 0 || !strings.HasSuffix(model, ")") {
		return model, false
	}
	suffix := model[open+1 : len(model)-1]
	if !isThinkingSuffix(suffix) {
		return model, false
	}
	return model[:open], true
}

func isThinkingSuffix(suffix string) bool {
	switch strings.ToLower(suffix) {
	case "none", "auto", "-1", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	budget, err := strconv.Atoi(suffix)
	return err == nil && budget >= 0
}
