package provider

import "github.com/Hamster-Prime/cpa-plugin-provider/internal/config"

const (
	ID              = "multi-protocol-provider"
	Version         = "0.1.0"
	DisplayName     = "Multi-Protocol Provider"
	Author          = "Hamster-Prime"
	Repository      = "https://github.com/Hamster-Prime/cpa-plugin-provider"
	CredentialsFile = "multi-protocol-provider.json"
)

// CloneConfig returns a deep copy suitable for capability instances that live
// across host callbacks while a later reconfigure builds a replacement plugin.
func CloneConfig(input config.Config) config.Config {
	out := input
	out.Headers = make(map[string]string, len(input.Headers))
	for key, value := range input.Headers {
		out.Headers[key] = value
	}
	out.Models = make([]config.Model, len(input.Models))
	for i, model := range input.Models {
		out.Models[i] = model
		out.Models[i].InputModalities = append([]string(nil), model.InputModalities...)
		out.Models[i].OutputModalities = append([]string(nil), model.OutputModalities...)
		if model.Thinking != nil {
			thinking := *model.Thinking
			thinking.Levels = append([]string(nil), model.Thinking.Levels...)
			out.Models[i].Thinking = &thinking
		}
	}
	return out
}
