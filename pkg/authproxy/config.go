package authproxy

// AuthConfig holds the full auth proxy configuration for a VM.
type AuthConfig struct {
	Providers []ProviderConfig `json:"providers"`
	Proxy     ProxyConfig      `json:"proxy"`
}

// ProviderConfig describes a single credential provider instance.
type ProviderConfig struct {
	Type   string            `json:"type"`   // "gcp-metadata", "github-app", "bearer-token", "delegated"
	Hosts  []string          `json:"hosts"`  // host glob patterns this provider handles
	Config map[string]string `json:"config"` // provider-specific key-value config
	Env    map[string]string `json:"env,omitempty"` // env vars to set in VM so tools attempt requests (e.g. GH_TOKEN)
}

// ProxyConfig holds settings for the HTTPS proxy.
type ProxyConfig struct {
	ListenPort   int      `json:"listen_port"`
	SSLBump      bool     `json:"ssl_bump"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// DefaultProxyConfig returns a ProxyConfig with sensible defaults.
func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{
		ListenPort: 3128,
		SSLBump:    true,
	}
}
