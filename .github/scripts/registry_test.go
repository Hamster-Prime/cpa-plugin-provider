package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
)

const customRegistryURL = "https://raw.githubusercontent.com/Hamster-Prime/cpa-plugin-provider/main/registry.json"

type customRegistry struct {
	SchemaVersion int                    `json:"schema_version"`
	Plugins       []customRegistryPlugin `json:"plugins"`
}

type customRegistryPlugin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Repository  string   `json:"repository"`
	Logo        string   `json:"logo"`
	Homepage    string   `json:"homepage"`
	License     string   `json:"license"`
	Tags        []string `json:"tags"`
}

func TestCustomRegistryMatchesPublishedPlugin(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "registry.json"))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}

	var registry customRegistry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&registry); err != nil {
		t.Fatalf("decode registry: %v", err)
	}
	if err = expectJSONEOF(decoder); err != nil {
		t.Fatalf("registry trailing data: %v", err)
	}
	if registry.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", registry.SchemaVersion)
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("plugins length = %d, want 1", len(registry.Plugins))
	}

	plugin := registry.Plugins[0]
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(plugin.ID) {
		t.Fatalf("invalid plugin id %q", plugin.ID)
	}
	if plugin.ID != provider.ID || plugin.Version != provider.Version {
		t.Fatalf("registry id/version = %q/%q, want %q/%q", plugin.ID, plugin.Version, provider.ID, provider.Version)
	}
	if strings.HasPrefix(plugin.Version, "v") {
		t.Fatalf("registry version must not use a v prefix: %q", plugin.Version)
	}
	if plugin.Name == "" || plugin.Description == "" || plugin.Author == "" {
		t.Fatalf("required registry metadata is incomplete: %#v", plugin)
	}
	if plugin.Repository != provider.Repository || plugin.Homepage != provider.Repository {
		t.Fatalf("registry repository/homepage = %q/%q, want %q", plugin.Repository, plugin.Homepage, provider.Repository)
	}
	if plugin.License != "MIT" || len(plugin.Tags) == 0 {
		t.Fatalf("registry license/tags = %q/%#v", plugin.License, plugin.Tags)
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !bytes.Contains(readme, []byte(customRegistryURL)) {
		t.Fatalf("README does not document custom registry URL %q", customRegistryURL)
	}
}

func expectJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}
