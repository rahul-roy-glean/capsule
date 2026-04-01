package accessplane

// Config holds the access plane connection configuration for a VM.
// This replaces the old authproxy.AuthConfig — instead of running a local proxy
// on the host, VMs connect to an external access plane service that handles
// credential injection, policy enforcement, and audit logging.
type Config struct {
	// APIEndpoint is the access plane HTTP API address (e.g. "http://access-plane:8080").
	APIEndpoint string `json:"api_endpoint"`

	// ProxyEndpoint is the access plane CONNECT proxy address (e.g. "access-plane:3128").
	ProxyEndpoint string `json:"proxy_endpoint"`

	// CACertPEM is the PEM-encoded CA certificate for SSL bump.
	// Installed in the VM's trust store so MITM'd connections are trusted.
	CACertPEM string `json:"ca_cert_pem,omitempty"`

	// PhantomFamilies lists tool families whose phantom env vars should be fetched
	// from the access plane (e.g. ["gcp_cli_read", "github_rest"]).
	PhantomFamilies []string `json:"phantom_families,omitempty"`

	// TenantID identifies the tenant this access plane serves.
	TenantID string `json:"tenant_id"`
}
