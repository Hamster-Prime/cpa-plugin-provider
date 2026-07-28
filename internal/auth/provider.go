package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Hamster-Prime/cpa-plugin-provider/internal/config"
	"github.com/Hamster-Prime/cpa-plugin-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// CredentialFile is the on-disk representation managed by the plugin UI. One
// file expands into one CPA auth candidate per key.
type CredentialFile struct {
	Type        string                 `json:"type"`
	Destination *CredentialDestination `json:"destination,omitempty"`
	Keys        []Key                  `json:"keys"`
}

// CredentialDestination binds a credential pool to the upstream for which its
// secrets were entered. Executors reject unbound or mismatched credentials.
type CredentialDestination struct {
	Protocol config.Protocol `json:"protocol"`
	BaseURL  string          `json:"base-url"`
}

func DestinationForConfig(cfg config.Config) CredentialDestination {
	cfg.Normalize()
	return CredentialDestination{Protocol: cfg.Protocol, BaseURL: cfg.BaseURL}
}

func (d *CredentialDestination) Normalize() {
	if d == nil {
		return
	}
	cfg := config.Default()
	cfg.Protocol = d.Protocol
	cfg.BaseURL = d.BaseURL
	cfg.Normalize()
	d.Protocol = cfg.Protocol
	d.BaseURL = cfg.BaseURL
}

func (d CredentialDestination) Validate() error {
	if strings.TrimSpace(string(d.Protocol)) == "" {
		return errors.New("destination.protocol is required")
	}
	if strings.TrimSpace(d.BaseURL) == "" {
		return errors.New("destination.base-url is required")
	}
	d.Normalize()
	cfg := config.Default()
	cfg.Protocol = d.Protocol
	cfg.BaseURL = d.BaseURL
	return cfg.Validate()
}

func (d CredentialDestination) MatchesConfig(cfg config.Config) bool {
	d.Normalize()
	target := DestinationForConfig(cfg)
	return d.Protocol == target.Protocol && d.BaseURL != "" && d.BaseURL == target.BaseURL
}

func (d CredentialDestination) Equal(other CredentialDestination) bool {
	d.Normalize()
	other.Normalize()
	return d.Protocol == other.Protocol && d.BaseURL != "" && d.BaseURL == other.BaseURL
}

// Key holds one independently schedulable upstream credential. Priority is an
// offset added to the provider-level priority from plugin configuration.
type Key struct {
	ID       string `json:"id,omitempty"`
	Label    string `json:"label,omitempty"`
	APIKey   string `json:"api-key,omitempty"`
	ProxyURL string `json:"proxy-url,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// UnmarshalJSON accepts the plugin's canonical fields and common legacy field
// spellings, while MarshalJSON always emits the canonical hyphenated fields.
func (k *Key) UnmarshalJSON(data []byte) error {
	type plain Key
	var raw struct {
		plain
		APIKeyUnderscore string `json:"api_key"`
		Key              string `json:"key"`
		Token            string `json:"token"`
		ProxyUnderscore  string `json:"proxy_url"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*k = Key(raw.plain)
	if k.APIKey == "" {
		k.APIKey = firstNonEmpty(raw.APIKeyUnderscore, raw.Key, raw.Token)
	}
	if k.ProxyURL == "" {
		k.ProxyURL = raw.ProxyUnderscore
	}
	return nil
}

type Provider struct {
	config config.Config
}

var _ pluginapi.AuthProvider = (*Provider)(nil)

func NewProvider(cfg config.Config) *Provider {
	cfg = provider.CloneConfig(cfg)
	cfg.Normalize()
	return &Provider{config: cfg}
}

func (p *Provider) Identifier() string { return provider.ID }

func (p *Provider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	credential, handled, err := ParseCredentialFile(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{Handled: handled}, err
	}
	if !handled {
		return pluginapi.AuthParseResponse{}, nil
	}

	auths, err := p.buildAuths(req.FileName, credential, req.Host.ProxyURL)
	if err != nil {
		return pluginapi.AuthParseResponse{Handled: true}, err
	}
	resp := pluginapi.AuthParseResponse{Handled: true, Auths: auths}
	if len(auths) > 0 {
		resp.Auth = auths[0]
	}
	return resp, nil
}

func (p *Provider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("%s uses API keys and has no interactive login flow", provider.ID)
}

func (p *Provider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError}, fmt.Errorf("%s uses API keys and has no interactive login flow", provider.ID)
}

func (p *Provider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	key, destination, hostProxyURL, err := parseStoredKey(req.StorageJSON)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	if strings.TrimSpace(req.Host.AuthDir) != "" {
		hostProxyURL = req.Host.ProxyURL
	}
	data, err := p.authData(req.AuthID, "", key, destination, hostProxyURL)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	for key, value := range req.Attributes {
		if _, exists := data.Attributes[key]; !exists {
			data.Attributes[key] = value
		}
	}
	return pluginapi.AuthRefreshResponse{Auth: data}, nil
}

// StorageMatchesConfig reports whether one reconciled auth record is bound to
// the provider's current protocol and upstream URL. It never exposes the key.
func StorageMatchesConfig(raw []byte, cfg config.Config) bool {
	_, destination, _, err := parseStoredKey(raw)
	return err == nil && destination != nil && destination.MatchesConfig(cfg)
}

// ParseCredentialFile decodes a plugin credential file. handled is false for
// valid JSON belonging to another provider, allowing CPA to try another parser.
func ParseCredentialFile(raw []byte) (credential CredentialFile, handled bool, err error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return CredentialFile{}, false, nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err = json.Unmarshal(raw, &probe); err != nil {
		return CredentialFile{}, false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(probe.Type), provider.ID) {
		return CredentialFile{}, false, nil
	}
	if err = json.Unmarshal(raw, &credential); err != nil {
		return CredentialFile{}, true, fmt.Errorf("decode %s credentials: %w", provider.ID, err)
	}
	credential.Normalize()
	if err = credential.Validate(); err != nil {
		return CredentialFile{}, true, err
	}
	return credential, true, nil
}

func MarshalCredentialFile(credential CredentialFile) ([]byte, error) {
	credential.Normalize()
	if err := credential.Validate(); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s credentials: %w", provider.ID, err)
	}
	return append(out, '\n'), nil
}

func (c *CredentialFile) Normalize() {
	if c == nil {
		return
	}
	c.Type = provider.ID
	if c.Destination != nil {
		destination := *c.Destination
		destination.Normalize()
		c.Destination = &destination
	}
	if c.Keys == nil {
		c.Keys = []Key{}
	}
	for i := range c.Keys {
		key := &c.Keys[i]
		key.ID = strings.TrimSpace(key.ID)
		if key.ID == "" {
			key.ID = fmt.Sprintf("key-%d", i+1)
		}
		key.Label = strings.TrimSpace(key.Label)
		key.APIKey = strings.TrimSpace(key.APIKey)
		key.ProxyURL = strings.TrimSpace(key.ProxyURL)
	}
}

func (c CredentialFile) Validate() error {
	if c.Type != provider.ID {
		return fmt.Errorf("credential type must be %q", provider.ID)
	}
	if c.Destination != nil {
		if err := c.Destination.Validate(); err != nil {
			return fmt.Errorf("credential destination: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(c.Keys))
	for i, key := range c.Keys {
		if key.ID == "" {
			return fmt.Errorf("keys[%d].id is required", i)
		}
		if _, exists := seen[key.ID]; exists {
			return fmt.Errorf("duplicate key id %q", key.ID)
		}
		seen[key.ID] = struct{}{}
		if err := validateProxyURL(key.ProxyURL); err != nil {
			return fmt.Errorf("keys[%d].proxy-url: %w", i, err)
		}
	}
	return nil
}

// MaskAPIKey returns a display-safe key preview and never returns the complete
// credential, including for short keys.
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 16 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", 8) + key[len(key)-4:]
}

// DisplayProxyURL returns a management-safe proxy setting and whether the
// stored value contains credentials that must be preserved separately.
func DisplayProxyURL(value string) (string, bool) {
	value = strings.TrimSpace(value)
	setting, err := proxyutil.Parse(value)
	if err != nil {
		return proxyutil.Redact(value), true
	}
	if setting.URL == nil {
		return value, false
	}
	sensitiveOrHidden := setting.URL.User != nil || setting.URL.Path != "" || setting.URL.RawQuery != "" || setting.URL.Fragment != ""
	return proxyutil.Redact(value), sensitiveOrHidden
}

func (p *Provider) buildAuths(fileName string, credential CredentialFile, hostProxyURL string) ([]pluginapi.AuthData, error) {
	fileName = filepath.Base(strings.TrimSpace(fileName))
	if fileName == "." || fileName == "" {
		fileName = provider.CredentialsFile
	}
	if err := validateProxyURL(hostProxyURL); err != nil {
		return nil, fmt.Errorf("host proxy URL: %w", err)
	}

	controller, errController := p.controllerAuthData(fileName, credential.Destination, hostProxyURL, len(credential.Keys) == 0)
	if errController != nil {
		return nil, errController
	}
	auths := make([]pluginapi.AuthData, 0, len(credential.Keys)+1)
	auths = append(auths, controller)
	for _, key := range credential.Keys {
		data, err := p.authData(authID(fileName, key.ID), fileName, key, credential.Destination, hostProxyURL)
		if err != nil {
			return nil, err
		}
		auths = append(auths, data)
	}
	return auths, nil
}

func (p *Provider) controllerAuthData(fileName string, destination *CredentialDestination, hostProxyURL string, empty bool) (pluginapi.AuthData, error) {
	stored, err := json.Marshal(struct {
		Type         string                 `json:"type"`
		Destination  *CredentialDestination `json:"destination,omitempty"`
		Controller   bool                   `json:"controller"`
		HostProxyURL string                 `json:"host-proxy-url,omitempty"`
	}{
		Type:         provider.ID,
		Destination:  destination,
		Controller:   true,
		HostProxyURL: strings.TrimSpace(hostProxyURL),
	})
	if err != nil {
		return pluginapi.AuthData{}, fmt.Errorf("encode %s controller storage: %w", provider.ID, err)
	}
	metadata := map[string]any{"controller": true}
	if empty {
		metadata["empty"] = true
	}
	return pluginapi.AuthData{
		Provider:    provider.ID,
		ID:          fileName,
		FileName:    fileName,
		Label:       p.config.Name,
		Disabled:    true,
		StorageJSON: stored,
		Metadata:    metadata,
		Attributes:  map[string]string{"auth_kind": "controller"},
	}, nil
}

func (p *Provider) authData(id, fileName string, key Key, destination *CredentialDestination, hostProxyURL string) (pluginapi.AuthData, error) {
	stored, err := json.Marshal(struct {
		Type         string                 `json:"type"`
		Destination  *CredentialDestination `json:"destination,omitempty"`
		ID           string                 `json:"id,omitempty"`
		Label        string                 `json:"label,omitempty"`
		APIKey       string                 `json:"api-key,omitempty"`
		ProxyURL     string                 `json:"proxy-url,omitempty"`
		HostProxyURL string                 `json:"host-proxy-url,omitempty"`
		Priority     int                    `json:"priority,omitempty"`
		Disabled     bool                   `json:"disabled,omitempty"`
	}{
		Type:         provider.ID,
		Destination:  destination,
		ID:           key.ID,
		Label:        key.Label,
		APIKey:       key.APIKey,
		ProxyURL:     key.ProxyURL,
		HostProxyURL: strings.TrimSpace(hostProxyURL),
		Priority:     key.Priority,
		Disabled:     key.Disabled,
	})
	if err != nil {
		return pluginapi.AuthData{}, fmt.Errorf("encode %s key storage: %w", provider.ID, err)
	}

	priority := p.config.Priority + key.Priority
	attributes := map[string]string{
		"auth_kind": "apikey",
		"key_id":    key.ID,
		"priority":  strconv.Itoa(priority),
	}
	metadata := map[string]any{
		"key_id":          key.ID,
		"priority":        priority,
		"disable_cooling": p.config.DisableCooling,
	}

	label := key.Label
	if label == "" {
		label = p.config.Name
	}
	// Provider-level disabling is represented by an empty model registration.
	// Persisting it on each auth would leave the keys disabled after the provider
	// is enabled again without reparsing the credential file.
	disabled := key.Disabled || key.APIKey == ""
	return pluginapi.AuthData{
		Provider:    provider.ID,
		ID:          strings.TrimSpace(id),
		FileName:    strings.TrimSpace(fileName),
		Label:       label,
		Prefix:      p.config.Prefix,
		ProxyURL:    key.ProxyURL,
		Disabled:    disabled,
		StorageJSON: stored,
		Metadata:    metadata,
		Attributes:  attributes,
	}, nil
}

func parseStoredKey(raw []byte) (Key, *CredentialDestination, string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Key{}, nil, "", fmt.Errorf("%s auth storage is empty", provider.ID)
	}
	var probe struct {
		Type         string                 `json:"type"`
		Destination  *CredentialDestination `json:"destination"`
		HostProxyURL string                 `json:"host-proxy-url"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Key{}, nil, "", fmt.Errorf("decode %s auth storage: %w", provider.ID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(probe.Type), provider.ID) {
		return Key{}, nil, "", fmt.Errorf("unexpected auth storage type %q", probe.Type)
	}
	var key Key
	if err := json.Unmarshal(raw, &key); err != nil {
		return Key{}, nil, "", fmt.Errorf("decode %s auth key: %w", provider.ID, err)
	}
	credential := CredentialFile{Type: provider.ID, Destination: probe.Destination, Keys: []Key{key}}
	credential.Normalize()
	if err := credential.Validate(); err != nil {
		return Key{}, nil, "", err
	}
	if err := validateProxyURL(probe.HostProxyURL); err != nil {
		return Key{}, nil, "", fmt.Errorf("host proxy URL: %w", err)
	}
	return credential.Keys[0], credential.Destination, strings.TrimSpace(probe.HostProxyURL), nil
}

func authID(fileName, keyID string) string {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		sum := sha256.Sum256([]byte(fileName))
		keyID = hex.EncodeToString(sum[:6])
	}
	return fileName + "#" + keyID
}

func validateProxyURL(value string) error {
	if _, err := proxyutil.Parse(value); err != nil {
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
