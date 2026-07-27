package provider

import (
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
)

func TestCloneConfigDoesNotShareMutableValues(t *testing.T) {
	original := config.Config{
		Headers: map[string]string{"X-Test": "one"},
		Models: []config.Model{{
			Name:             "model",
			InputModalities:  []string{"text"},
			OutputModalities: []string{"text"},
			Thinking:         &config.ThinkingSupport{Levels: []string{"low"}},
		}},
	}
	cloned := CloneConfig(original)
	cloned.Headers["X-Test"] = "two"
	cloned.Models[0].Name = "changed"
	cloned.Models[0].InputModalities[0] = "image"
	cloned.Models[0].OutputModalities[0] = "image"
	cloned.Models[0].Thinking.Levels[0] = "high"

	if original.Headers["X-Test"] != "one" || original.Models[0].Name != "model" || original.Models[0].InputModalities[0] != "text" || original.Models[0].OutputModalities[0] != "text" || original.Models[0].Thinking.Levels[0] != "low" {
		t.Fatalf("CloneConfig shared mutable state with original: %#v", original)
	}
}
