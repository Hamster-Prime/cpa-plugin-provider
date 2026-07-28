package provider

import (
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
)

func TestPublishedMetadata(t *testing.T) {
	if ID != "multi-protocol-provider" {
		t.Fatalf("ID = %q", ID)
	}
	if Version != "0.2.0" {
		t.Fatalf("Version = %q", Version)
	}
	if DisplayName != "Multi-Protocol Provider" || Author != "Hamster-Prime" || Repository != "https://github.com/Hamster-Prime/cpa-plugin-provider" {
		t.Fatalf("published metadata is inconsistent: name=%q author=%q repository=%q", DisplayName, Author, Repository)
	}
}

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
