package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	providerinfo "github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseAuthExpandsKeysWithoutLeakingSecrets(t *testing.T) {
	p := NewProvider(config.Config{
		Name:           "Acme",
		Prefix:         "team",
		Priority:       10,
		DisableCooling: true,
	})
	raw := []byte(`{
		"type":"multi-protocol-provider",
		"destination":{"protocol":"anthropic-messages","base-url":"https://api.example.com/v1/"},
		"keys":[
			{"id":"primary","label":"Main","api-key":"sk-primary-secret","proxy-url":"https://proxy.example","priority":2},
			{"id":"backup","api_key":"sk-backup-secret","priority":-3,"disabled":true}
		]
	}`)
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON: raw, FileName: "provider.json",
		Host: pluginapi.HostConfigSummary{ProxyURL: "socks5://global-proxy.example:1080"},
	})
	if err != nil {
		t.Fatalf("ParseAuth() error = %v", err)
	}
	if !resp.Handled || len(resp.Auths) != 3 {
		t.Fatalf("ParseAuth() handled=%v auths=%d, want true/3", resp.Handled, len(resp.Auths))
	}

	controller, primary, backup := resp.Auths[0], resp.Auths[1], resp.Auths[2]
	if controller.ID != "provider.json" || controller.FileName != "provider.json" || !controller.Disabled || controller.Metadata["controller"] != true {
		t.Fatalf("controller = %#v", controller)
	}
	if strings.Contains(string(controller.StorageJSON), "api-key") || strings.Contains(string(controller.StorageJSON), "sk-") {
		t.Fatalf("controller storage contains a secret: %s", controller.StorageJSON)
	}
	if primary.Provider != providerinfo.ID || primary.ID != "provider.json#primary" || primary.Label != "Main" {
		t.Fatalf("primary identity = %#v", primary)
	}
	if primary.Prefix != "team" || primary.ProxyURL != "https://proxy.example" || primary.Disabled {
		t.Fatalf("primary routing fields = %#v", primary)
	}
	if primary.Attributes["priority"] != "12" || primary.Metadata["priority"] != 12 {
		t.Fatalf("primary priority attrs=%#v metadata=%#v", primary.Attributes, primary.Metadata)
	}
	if primary.Metadata["disable_cooling"] != true {
		t.Fatalf("disable_cooling = %#v", primary.Metadata["disable_cooling"])
	}
	if backup.Attributes["priority"] != "7" || backup.Attributes["runtime_only"] != "" || !backup.Disabled {
		t.Fatalf("backup fields = %#v", backup)
	}
	if backup.ID != "provider.json#backup" {
		t.Fatalf("backup ID = %q", backup.ID)
	}
	reordered, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON:  []byte(`{"type":"multi-protocol-provider","keys":[{"id":"backup","api-key":"b"},{"id":"primary","api-key":"p"}]}`),
		FileName: "provider.json",
	})
	if err != nil || len(reordered.Auths) != 3 || reordered.Auths[0].ID != controller.ID || reordered.Auths[1].ID != backup.ID || reordered.Auths[2].ID != primary.ID {
		t.Fatalf("reordered auth identities = %#v, error = %v", reordered.Auths, err)
	}

	for _, candidate := range resp.Auths {
		safe, errMarshal := json.Marshal(struct {
			Metadata   map[string]any
			Attributes map[string]string
		}{candidate.Metadata, candidate.Attributes})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(safe), "sk-") {
			t.Fatalf("secret leaked outside StorageJSON: %s", safe)
		}
	}
	if !strings.Contains(string(primary.StorageJSON), "sk-primary-secret") {
		t.Fatalf("StorageJSON does not contain selected key: %s", primary.StorageJSON)
	}
	if strings.Contains(string(primary.StorageJSON), "sk-backup-secret") {
		t.Fatalf("StorageJSON contains another key: %s", primary.StorageJSON)
	}
	if !strings.Contains(string(primary.StorageJSON), `"protocol":"anthropic-messages"`) ||
		!strings.Contains(string(primary.StorageJSON), `"base-url":"https://api.example.com/v1"`) {
		t.Fatalf("StorageJSON does not retain normalized credential destination: %s", primary.StorageJSON)
	}
	if !strings.Contains(string(primary.StorageJSON), `"host-proxy-url":"socks5://global-proxy.example:1080"`) {
		t.Fatalf("StorageJSON does not retain the host proxy fallback: %s", primary.StorageJSON)
	}
}

func TestParseAuthReturnsDisabledPlaceholderForEmptyKeyList(t *testing.T) {
	p := NewProvider(config.Config{Name: "Empty provider", Priority: 4})
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON:  []byte(`{"type":"multi-protocol-provider","keys":[]}`),
		FileName: providerinfo.CredentialsFile,
	})
	if err != nil {
		t.Fatalf("ParseAuth() error = %v", err)
	}
	if !resp.Handled || len(resp.Auths) != 1 || !resp.Auths[0].Disabled {
		t.Fatalf("empty ParseAuth() = %#v", resp)
	}
	if resp.Auths[0].Metadata["empty"] != true || strings.Contains(string(resp.Auths[0].StorageJSON), "api-key") {
		t.Fatalf("placeholder metadata/storage = %#v / %s", resp.Auths[0].Metadata, resp.Auths[0].StorageJSON)
	}
}

func TestProviderDisabledDoesNotPersistOnUsableKey(t *testing.T) {
	p := NewProvider(config.Config{Name: "Temporarily disabled", Disabled: true})
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON:  []byte(`{"type":"multi-protocol-provider","keys":[{"id":"primary","api-key":"secret"}]}`),
		FileName: providerinfo.CredentialsFile,
	})
	if err != nil {
		t.Fatalf("ParseAuth() error = %v", err)
	}
	if len(resp.Auths) != 2 || resp.Auths[0].Metadata["controller"] != true || !resp.Auths[0].Disabled || resp.Auths[1].Disabled {
		t.Fatalf("provider-level disabled leaked into auth = %#v", resp.Auths)
	}
}

func TestCredentialFileCompatibilityAndValidation(t *testing.T) {
	credential, handled, err := ParseCredentialFile([]byte(`{
		"type":"multi-protocol-provider",
		"keys":[{"token":"secret","proxy_url":"http://proxy.example"}]
	}`))
	if err != nil || !handled {
		t.Fatalf("ParseCredentialFile() handled=%v error=%v", handled, err)
	}
	if credential.Keys[0].ID != "key-1" || credential.Keys[0].APIKey != "secret" || credential.Keys[0].ProxyURL != "http://proxy.example" {
		t.Fatalf("credential = %#v", credential)
	}
	encoded, err := MarshalCredentialFile(credential)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"api-key": "secret"`) || strings.Contains(string(encoded), "api_key") {
		t.Fatalf("canonical JSON = %s", encoded)
	}

	_, handled, err = ParseCredentialFile([]byte(`{"type":"other","keys":[]}`))
	if err != nil || handled {
		t.Fatalf("other provider handled=%v error=%v", handled, err)
	}
	_, handled, err = ParseCredentialFile([]byte(`{"type":"multi-protocol-provider","keys":[{"id":"x"},{"id":"x"}]}`))
	if err == nil || !handled || !strings.Contains(err.Error(), "duplicate key id") {
		t.Fatalf("duplicate handled=%v error=%v", handled, err)
	}
	for _, proxyURL := range []string{
		"direct",
		"none",
		"socks5://proxy.example:1080",
		"socks5h://user:password@proxy.example:1080",
		"http://user:password@proxy.example:8080",
	} {
		_, handled, err = ParseCredentialFile([]byte(`{"type":"multi-protocol-provider","keys":[{"proxy-url":"` + proxyURL + `"}]}`))
		if err != nil || !handled {
			t.Fatalf("proxy %q handled=%v error=%v", proxyURL, handled, err)
		}
	}
	_, handled, err = ParseCredentialFile([]byte(`{"type":"multi-protocol-provider","keys":[{"proxy-url":"ftp://proxy.example"}]}`))
	if err == nil || !handled || !strings.Contains(err.Error(), "unsupported proxy scheme") {
		t.Fatalf("invalid proxy handled=%v error=%v", handled, err)
	}
}

func TestDisplayProxyURLRedactsAllNonOriginComponents(t *testing.T) {
	display, secret := DisplayProxyURL("http://proxy-user:proxy-password@proxy.example:8080/path?q=secret")
	if !secret || display != "http://redacted@proxy.example:8080" {
		t.Fatalf("DisplayProxyURL() = %q, %v", display, secret)
	}
	if display, secret = DisplayProxyURL("http://proxy.example:8080/token-path?q=secret"); !secret || display != "http://proxy.example:8080" {
		t.Fatalf("DisplayProxyURL(path/query) = %q, %v", display, secret)
	}
	if display, secret = DisplayProxyURL("socks5://proxy.example:1080"); secret || display != "socks5://proxy.example:1080" {
		t.Fatalf("DisplayProxyURL(no auth) = %q, %v", display, secret)
	}
	if display, secret = DisplayProxyURL("direct"); secret || display != "direct" {
		t.Fatalf("DisplayProxyURL(direct) = %q, %v", display, secret)
	}
}

func TestRefreshAuthReturnsSameAPIKeyAndRoutingMetadata(t *testing.T) {
	p := NewProvider(config.Config{Name: "Acme", Priority: 5, DisableCooling: true})
	resp, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID:      "provider.json#primary",
		StorageJSON: []byte(`{"type":"multi-protocol-provider","destination":{"protocol":"gemini","base-url":"https://api.example.com/v1beta"},"id":"primary","api-key":"secret","host-proxy-url":"http://old-proxy.example:8080","priority":3}`),
		Attributes:  map[string]string{"runtime_only": "true", "custom": "value"},
		Host:        pluginapi.HostConfigSummary{AuthDir: "/auth", ProxyURL: "direct"},
	})
	if err != nil {
		t.Fatalf("RefreshAuth() error = %v", err)
	}
	if resp.Auth.ID != "provider.json#primary" || resp.Auth.Attributes["priority"] != "8" || resp.Auth.Attributes["custom"] != "value" {
		t.Fatalf("RefreshAuth() = %#v", resp.Auth)
	}
	if !strings.Contains(string(resp.Auth.StorageJSON), `"api-key":"secret"`) {
		t.Fatalf("refreshed storage = %s", resp.Auth.StorageJSON)
	}
	if !strings.Contains(string(resp.Auth.StorageJSON), `"protocol":"gemini"`) || !strings.Contains(string(resp.Auth.StorageJSON), `"base-url":"https://api.example.com/v1beta"`) {
		t.Fatalf("refreshed storage lost credential destination = %s", resp.Auth.StorageJSON)
	}
	if !strings.Contains(string(resp.Auth.StorageJSON), `"host-proxy-url":"direct"`) || strings.Contains(string(resp.Auth.StorageJSON), "old-proxy") {
		t.Fatalf("refreshed storage did not update the host proxy fallback = %s", resp.Auth.StorageJSON)
	}
}

func TestMaskAPIKeyNeverReturnsCompleteShortSecret(t *testing.T) {
	for _, secret := range []string{"short", "123456789", "1234567890123456"} {
		if got := MaskAPIKey(secret); got != strings.Repeat("*", len(secret)) {
			t.Fatalf("MaskAPIKey(%q) = %q", secret, got)
		}
	}
	if got := MaskAPIKey("sk-1234567890-secret"); got != "sk-1********cret" {
		t.Fatalf("MaskAPIKey(long) = %q", got)
	}
}
