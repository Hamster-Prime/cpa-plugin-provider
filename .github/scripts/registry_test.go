package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
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
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Author      string                `json:"author"`
	Version     string                `json:"version"`
	Repository  string                `json:"repository"`
	Logo        string                `json:"logo"`
	Homepage    string                `json:"homepage"`
	License     string                `json:"license"`
	Tags        []string              `json:"tags"`
	Install     customRegistryInstall `json:"install"`
}

type customRegistryInstall struct {
	Type      string                   `json:"type"`
	Artifacts []customRegistryArtifact `json:"artifacts"`
}

type customRegistryArtifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func TestCustomRegistryIsPinnedAndSelfConsistent(t *testing.T) {
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
	if registry.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", registry.SchemaVersion)
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("plugins length = %d, want 1", len(registry.Plugins))
	}

	plugin := registry.Plugins[0]
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(plugin.ID) {
		t.Fatalf("invalid plugin id %q", plugin.ID)
	}
	if plugin.ID != provider.ID {
		t.Fatalf("registry id = %q, want %q", plugin.ID, provider.ID)
	}
	if !regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+$`).MatchString(plugin.Version) {
		t.Fatalf("registry version must be a numeric version without a v prefix: %q", plugin.Version)
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
	if plugin.Install.Type != "direct" {
		t.Fatalf("registry install.type = %q, want direct", plugin.Install.Type)
	}
	wantArtifacts := map[string]customRegistryArtifact{
		"linux/amd64": {
			SHA256: "07cc59829a279e6766f0f9be853688593456f0c8a4ec2a6468d2b5615b099c09",
			Size:   5024708,
		},
		"linux/arm64": {
			SHA256: "fc048a4c83c04d8982318b66d6b6aef724568d68eed1511dc48bb360c0accd77",
			Size:   4561697,
		},
		"darwin/amd64": {
			SHA256: "a98a3145f1005290c8b659da8f25338a2a61a3907a2dff77c3668101f143fff4",
			Size:   4806151,
		},
		"darwin/arm64": {
			SHA256: "b44bedec07e9f56d92a1b9af6151dcb99ab611a9feda77c4b2c6ebf3293207a9",
			Size:   4439019,
		},
		"windows/amd64": {
			SHA256: "8361567753897f087ae76d6ddf690856927fc3bbea14fe33da1d371e36d14f05",
			Size:   4902144,
		},
	}
	if len(plugin.Install.Artifacts) != len(wantArtifacts) {
		t.Fatalf("registry artifact count = %d, want %d", len(plugin.Install.Artifacts), len(wantArtifacts))
	}
	for _, artifact := range plugin.Install.Artifacts {
		key := artifact.GOOS + "/" + artifact.GOARCH
		want, ok := wantArtifacts[key]
		if !ok {
			t.Fatalf("unexpected registry artifact platform %q", key)
		}
		if artifact.SHA256 != want.SHA256 || artifact.Size != want.Size {
			t.Fatalf("artifact %s hash/size = %q/%d, want %q/%d", key, artifact.SHA256, artifact.Size, want.SHA256, want.Size)
		}
		parsed, errParse := url.Parse(artifact.URL)
		if errParse != nil || parsed.Scheme != "https" || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatalf("artifact %s URL is not a pinned HTTPS URL: %q", key, artifact.URL)
		}
		if !strings.Contains(parsed.Path, "/releases/download/v"+plugin.Version+"/") {
			t.Fatalf("artifact %s URL does not point to v%s release: %q", key, plugin.Version, artifact.URL)
		}
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
