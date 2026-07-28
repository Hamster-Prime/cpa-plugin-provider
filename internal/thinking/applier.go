package thinking

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Applier struct {
	protocol config.Protocol
}

var _ pluginapi.ThinkingApplier = (*Applier)(nil)

func NewApplier(cfg config.Config) *Applier {
	cfg = provider.CloneConfig(cfg)
	cfg.Normalize()
	return &Applier{protocol: cfg.Protocol}
}

func (a *Applier) Identifier() string { return provider.ID }

// ApplyRequestedModel applies the canonical CPA thinking suffix carried by the
// original requested model. Stock hosts may not invoke plugin thinking appliers,
// so the executor uses this as a request-local compatibility path.
func ApplyRequestedModel(ctx context.Context, protocol config.Protocol, body []byte, requestedModel string, model pluginapi.ModelInfo) ([]byte, error) {
	cfg, hasSuffix, err := thinkingConfigFromSuffix(requestedModel, model.Thinking, protocol)
	if err != nil || !hasSuffix {
		return append([]byte(nil), body...), err
	}
	returnBody, errApply := (&Applier{protocol: protocol}).ApplyThinking(ctx, pluginapi.ThinkingApplyRequest{
		Provider: provider.ID,
		Model:    model,
		Config:   cfg,
		Body:     body,
	})
	if errApply != nil {
		return append([]byte(nil), body...), errApply
	}
	return returnBody.Body, nil
}

func (a *Applier) ApplyThinking(_ context.Context, req pluginapi.ThinkingApplyRequest) (pluginapi.PayloadResponse, error) {
	body := append([]byte(nil), req.Body...)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}
	cfg := normalize(req.Config)

	switch a.protocol {
	case config.ProtocolOpenAIResponses:
		body = applyEffort(body, "reasoning.effort", cfg)
	case config.ProtocolAnthropic:
		body = applyClaude(body, cfg, req.Model)
	case config.ProtocolGemini:
		body = applyGemini(body, cfg)
	default:
		body = applyEffort(body, "reasoning_effort", cfg)
	}
	return pluginapi.PayloadResponse{Body: body}, nil
}

func normalize(cfg pluginapi.ThinkingConfig) pluginapi.ThinkingConfig {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	cfg.Level = strings.ToLower(strings.TrimSpace(cfg.Level))
	switch cfg.Mode {
	case "none", "budget", "level", "auto":
	default:
		cfg.Mode = "auto"
	}
	if cfg.Mode == "level" && cfg.Level == "" {
		cfg.Mode = "auto"
	}
	if cfg.Mode == "budget" && cfg.Budget == 0 {
		cfg.Mode = "none"
	}
	return cfg
}

func thinkingConfigFromSuffix(requestedModel string, support *pluginapi.ThinkingSupport, protocol config.Protocol) (pluginapi.ThinkingConfig, bool, error) {
	open := strings.LastIndex(requestedModel, "(")
	if open <= 0 || !strings.HasSuffix(requestedModel, ")") {
		return pluginapi.ThinkingConfig{}, false, nil
	}
	suffix := strings.ToLower(requestedModel[open+1 : len(requestedModel)-1])
	var cfg pluginapi.ThinkingConfig
	switch suffix {
	case "none":
		cfg.Mode = "none"
	case "auto", "-1":
		cfg.Mode = "auto"
	case "minimal", "low", "medium", "high", "xhigh", "max":
		cfg.Mode, cfg.Level = "level", suffix
	default:
		budget, err := strconv.Atoi(suffix)
		if err != nil || budget < 0 {
			return pluginapi.ThinkingConfig{}, false, nil
		}
		if budget == 0 {
			cfg.Mode = "none"
		} else {
			cfg.Mode, cfg.Budget = "budget", budget
		}
	}
	if support == nil {
		return pluginapi.ThinkingConfig{}, true, fmt.Errorf("model %q does not support thinking", requestedModel[:open])
	}
	return normalizeSuffixConfig(cfg, support, protocol)
}

func normalizeSuffixConfig(cfg pluginapi.ThinkingConfig, support *pluginapi.ThinkingSupport, protocol config.Protocol) (pluginapi.ThinkingConfig, bool, error) {
	hasBudget := support.Min > 0 || support.Max > 0
	hasLevels := len(support.Levels) > 0
	switch cfg.Mode {
	case "level":
		if hasLevels {
			if !hasThinkingLevel(support.Levels, cfg.Level) {
				return pluginapi.ThinkingConfig{}, true, fmt.Errorf("thinking level %q is not supported", cfg.Level)
			}
			return cfg, true, nil
		}
		if hasBudget {
			budget, ok := levelToBudget(cfg.Level)
			if !ok {
				return pluginapi.ThinkingConfig{}, true, fmt.Errorf("unknown thinking level %q", cfg.Level)
			}
			cfg.Mode, cfg.Level, cfg.Budget = "budget", "", clampSuffixBudget(budget, support)
		}
	case "budget":
		if hasLevels && !hasBudget {
			cfg.Mode, cfg.Level, cfg.Budget = "level", nearestThinkingLevel(budgetToLevel(cfg.Budget), support.Levels), 0
		} else {
			cfg.Budget = clampSuffixBudget(cfg.Budget, support)
		}
	case "auto":
		if support.DynamicAllowed {
			return cfg, true, nil
		}
		if hasLevels && !hasBudget {
			cfg.Mode, cfg.Level = "level", support.Levels[len(support.Levels)/2]
		} else {
			cfg.Mode = "budget"
			cfg.Budget = (support.Min + support.Max) / 2
			if cfg.Budget <= 0 {
				if support.ZeroAllowed {
					cfg.Mode = "none"
				} else {
					cfg.Budget = support.Min
				}
			}
		}
	case "none":
		if protocol == config.ProtocolAnthropic || support.ZeroAllowed || hasThinkingLevel(support.Levels, "none") {
			return cfg, true, nil
		}
		if hasLevels {
			cfg.Level = strings.ToLower(strings.TrimSpace(support.Levels[0]))
		} else if hasBudget {
			cfg.Budget = clampSuffixBudget(0, support)
		}
	}
	return cfg, true, nil
}

func clampSuffixBudget(value int, support *pluginapi.ThinkingSupport) int {
	if value == 0 && !support.ZeroAllowed {
		value = support.Min
	}
	if support.Min > 0 && value < support.Min {
		value = support.Min
	}
	if support.Max > 0 && value > support.Max {
		value = support.Max
	}
	return value
}

func levelToBudget(level string) (int, bool) {
	values := map[string]int{
		"none": 0, "auto": -1, "minimal": 512, "low": 1024,
		"medium": 8192, "high": 24576, "xhigh": 32768, "max": 128000,
	}
	value, ok := values[strings.ToLower(strings.TrimSpace(level))]
	return value, ok
}

func hasThinkingLevel(levels []string, target string) bool {
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), target) {
			return true
		}
	}
	return false
}

func nearestThinkingLevel(level string, supported []string) string {
	if len(supported) == 0 || hasThinkingLevel(supported, level) {
		return level
	}
	order := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	position := func(value string) int {
		for index, candidate := range order {
			if strings.EqualFold(strings.TrimSpace(value), candidate) {
				return index
			}
		}
		return -1
	}
	wanted := position(level)
	best, distance := strings.ToLower(strings.TrimSpace(supported[0])), len(order)+1
	for _, candidate := range supported {
		index := position(candidate)
		if index < 0 {
			continue
		}
		candidateDistance := index - wanted
		if candidateDistance < 0 {
			candidateDistance = -candidateDistance
		}
		if candidateDistance < distance {
			best, distance = order[index], candidateDistance
		}
	}
	return best
}

func applyEffort(body []byte, path string, cfg pluginapi.ThinkingConfig) []byte {
	effort := ""
	switch cfg.Mode {
	case "none":
		effort = cfg.Level
		if effort == "" && cfg.Budget > 0 {
			effort = budgetToLevel(cfg.Budget)
		}
		if effort == "" {
			effort = "none"
		}
	case "auto":
		effort = "auto"
	case "level":
		effort = cfg.Level
	case "budget":
		effort = budgetToLevel(cfg.Budget)
	}
	if effort == "" {
		return body
	}
	out, err := sjson.SetBytes(body, path, effort)
	if err != nil {
		return body
	}
	return out
}

func applyClaude(body []byte, cfg pluginapi.ThinkingConfig, model pluginapi.ModelInfo) []byte {
	switch cfg.Mode {
	case "none":
		body = set(body, "thinking.type", "disabled")
		body = deletePath(body, "thinking.budget_tokens")
		body = deletePath(body, "output_config.effort")
		return deleteEmptyObject(body, "output_config")
	case "level":
		body = set(body, "thinking.type", "adaptive")
		body = deletePath(body, "thinking.budget_tokens")
		return set(body, "output_config.effort", cfg.Level)
	case "auto":
		body = set(body, "thinking.type", "adaptive")
		body = deletePath(body, "thinking.budget_tokens")
		body = deletePath(body, "output_config.effort")
		return deleteEmptyObject(body, "output_config")
	case "budget":
		budget := clampBudget(cfg.Budget, model.Thinking)
		if budget <= 0 {
			return applyClaude(body, pluginapi.ThinkingConfig{Mode: "none"}, model)
		}
		if maxTokens := int(gjson.GetBytes(body, "max_tokens").Int()); maxTokens > 0 && budget >= maxTokens {
			if model.Thinking != nil && model.Thinking.Min > 0 && maxTokens <= model.Thinking.Min {
				return applyClaude(body, pluginapi.ThinkingConfig{Mode: "none"}, model)
			}
			budget = maxTokens - 1
		}
		if budget <= 0 {
			return applyClaude(body, pluginapi.ThinkingConfig{Mode: "none"}, model)
		}
		body = set(body, "thinking.type", "enabled")
		body = set(body, "thinking.budget_tokens", budget)
		body = deletePath(body, "output_config.effort")
		return deleteEmptyObject(body, "output_config")
	default:
		return body
	}
}

func applyGemini(body []byte, cfg pluginapi.ThinkingConfig) []byte {
	const base = "generationConfig.thinkingConfig"
	switch cfg.Mode {
	case "none":
		if cfg.Level == "" && cfg.Budget <= 0 {
			return deletePath(body, base)
		}
		if cfg.Level != "" {
			body = deletePath(body, base+".thinkingBudget")
			body = set(body, base+".thinkingLevel", cfg.Level)
		} else {
			body = deletePath(body, base+".thinkingLevel")
			body = set(body, base+".thinkingBudget", cfg.Budget)
		}
		return set(body, base+".includeThoughts", false)
	case "level":
		body = deletePath(body, base+".thinkingBudget")
		body = set(body, base+".thinkingLevel", cfg.Level)
		return setDefaultBool(body, base+".includeThoughts", true)
	case "auto":
		body = deletePath(body, base+".thinkingLevel")
		body = set(body, base+".thinkingBudget", -1)
		return setDefaultBool(body, base+".includeThoughts", true)
	case "budget":
		body = deletePath(body, base+".thinkingLevel")
		body = set(body, base+".thinkingBudget", cfg.Budget)
		return setDefaultBool(body, base+".includeThoughts", cfg.Budget > 0)
	default:
		return body
	}
}

func budgetToLevel(budget int) string {
	switch {
	case budget < -1:
		return ""
	case budget == -1:
		return "auto"
	case budget == 0:
		return "none"
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	case budget <= 32768:
		return "xhigh"
	default:
		return "max"
	}
}

func clampBudget(budget int, support *pluginapi.ThinkingSupport) int {
	if support == nil || budget <= 0 {
		return budget
	}
	if support.Min > 0 && budget < support.Min {
		budget = support.Min
	}
	if support.Max > 0 && budget > support.Max {
		budget = support.Max
	}
	return budget
}

func set(body []byte, path string, value any) []byte {
	out, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return out
}

func deletePath(body []byte, path string) []byte {
	out, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body
	}
	return out
}

func deleteEmptyObject(body []byte, path string) []byte {
	value := gjson.GetBytes(body, path)
	if value.Exists() && value.IsObject() && len(value.Map()) == 0 {
		return deletePath(body, path)
	}
	return body
}

func setDefaultBool(body []byte, path string, fallback bool) []byte {
	if existing := gjson.GetBytes(body, path); existing.Exists() {
		fallback = existing.Bool()
	}
	return set(body, path, fallback)
}
