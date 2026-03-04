package authproxy

import "fmt"

// ProviderFactory creates a CredentialProvider from configuration.
type ProviderFactory func(cfg ProviderConfig) (CredentialProvider, error)

var registry = map[string]ProviderFactory{}

// RegisterProvider adds a provider factory to the global registry.
// Call this from provider init() functions.
func RegisterProvider(name string, factory ProviderFactory) {
	registry[name] = factory
}

// GetRegisteredProvider returns the factory for a given provider type.
// Useful for testing that init() registration occurred.
func GetRegisteredProvider(name string) (ProviderFactory, bool) {
	f, ok := registry[name]
	return f, ok
}

// BuildProviders instantiates all configured providers using the registry.
func BuildProviders(configs []ProviderConfig) ([]CredentialProvider, error) {
	var providers []CredentialProvider
	for _, cfg := range configs {
		factory, ok := registry[cfg.Type]
		if !ok {
			return nil, fmt.Errorf("unknown provider type: %q", cfg.Type)
		}
		p, err := factory(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider %q: %w", cfg.Type, err)
		}
		providers = append(providers, p)
	}
	return providers, nil
}
