package thinking

import (
	"context"
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
