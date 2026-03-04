package authproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// --- helper: mock provider ---

type mockProvider struct {
	name     string
	hosts    []string
	token    string
	header   string
	startErr error
	stopped  bool
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Matches(host string) bool {
	for _, h := range m.hosts {
		if h == host {
			return true
		}
	}
	return false
}
func (m *mockProvider) InjectCredentials(req *http.Request) error {
	if m.token == "" {
		return fmt.Errorf("no token")
	}
	h := m.header
	if h == "" {
		h = "Authorization"
	}
	req.Header.Set(h, m.token)
	return nil
}
func (m *mockProvider) Start(_ context.Context) error { return m.startErr }
func (m *mockProvider) Stop()                         { m.stopped = true }

// mockDelegatedProvider implements both CredentialProvider and TokenReceiver.
type mockDelegatedProvider struct {
	mockProvider
	pushedToken     string
	pushedExpiresAt time.Time
}

func (m *mockDelegatedProvider) UpdateToken(token string, expiresAt time.Time) error {
	m.pushedToken = token
	m.pushedExpiresAt = expiresAt
	return nil
}

// mockMetadataProvider implements CredentialProvider and MetadataHandler.
type mockMetadataProvider struct {
	mockProvider
	servedPaths []string
}

func (m *mockMetadataProvider) ServeMetadata(w http.ResponseWriter, r *http.Request) {
	m.servedPaths = append(m.servedPaths, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
}

// --- Config tests ---

func TestDefaultProxyConfig(t *testing.T) {
	cfg := DefaultProxyConfig()
	if cfg.ListenPort != 3128 {
		t.Errorf("ListenPort = %d, want 3128", cfg.ListenPort)
	}
	if !cfg.SSLBump {
		t.Error("SSLBump = false, want true")
	}
}

func TestAuthConfig_JSONRoundTrip(t *testing.T) {
	original := AuthConfig{
		Providers: []ProviderConfig{
			{
				Type:  "github-app",
				Hosts: []string{"github.com", "api.github.com"},
				Config: map[string]string{
					"app_id":     "12345",
					"secret_ref": "sm://project/secret",
				},
			},
			{
				Type:  "delegated",
				Hosts: []string{"registry.npmjs.org"},
				Config: map[string]string{
					"header": "Authorization",
					"prefix": "Bearer ",
				},
			},
		},
		Proxy: ProxyConfig{
			ListenPort:   3128,
			SSLBump:      true,
			AllowedHosts: []string{"github.com", "*.googleapis.com"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded AuthConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Providers) != 2 {
		t.Fatalf("Providers len = %d, want 2", len(decoded.Providers))
	}
	if decoded.Providers[0].Type != "github-app" {
		t.Errorf("Providers[0].Type = %q, want %q", decoded.Providers[0].Type, "github-app")
	}
	if decoded.Providers[0].Config["app_id"] != "12345" {
		t.Errorf("Providers[0].Config[app_id] = %q, want %q", decoded.Providers[0].Config["app_id"], "12345")
	}
	if decoded.Proxy.ListenPort != 3128 {
		t.Errorf("Proxy.ListenPort = %d, want 3128", decoded.Proxy.ListenPort)
	}
	if len(decoded.Proxy.AllowedHosts) != 2 {
		t.Errorf("Proxy.AllowedHosts len = %d, want 2", len(decoded.Proxy.AllowedHosts))
	}
}

// --- Registry tests ---

func TestBuildProviders_UnknownType(t *testing.T) {
	_, err := BuildProviders([]ProviderConfig{
		{Type: "nonexistent"},
	})
	if err == nil {
		t.Fatal("expected error for unknown provider type")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q, want to mention %q", err.Error(), "nonexistent")
	}
}

func TestBuildProviders_EmptyConfig(t *testing.T) {
	providers, err := BuildProviders(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("providers len = %d, want 0", len(providers))
	}
}

func TestRegisterAndBuildProvider(t *testing.T) {
	// Register a test provider.
	RegisterProvider("test-provider", func(cfg ProviderConfig) (CredentialProvider, error) {
		return &mockProvider{
			name:  "test-provider",
			hosts: cfg.Hosts,
			token: cfg.Config["token"],
		}, nil
	})
	defer delete(registry, "test-provider")

	providers, err := BuildProviders([]ProviderConfig{
		{
			Type:   "test-provider",
			Hosts:  []string{"example.com"},
			Config: map[string]string{"token": "secret123"},
		},
	})
	if err != nil {
		t.Fatalf("BuildProviders failed: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(providers))
	}
	if providers[0].Name() != "test-provider" {
		t.Errorf("Name() = %q, want %q", providers[0].Name(), "test-provider")
	}
	if !providers[0].Matches("example.com") {
		t.Error("expected Matches(example.com) = true")
	}
}

func TestBuildProviders_FactoryError(t *testing.T) {
	RegisterProvider("fail-provider", func(_ ProviderConfig) (CredentialProvider, error) {
		return nil, fmt.Errorf("factory boom")
	})
	defer delete(registry, "fail-provider")

	_, err := BuildProviders([]ProviderConfig{
		{Type: "fail-provider"},
	})
	if err == nil {
		t.Fatal("expected error from factory")
	}
	if !strings.Contains(err.Error(), "factory boom") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "factory boom")
	}
}

// --- matchHostGlob tests ---

func TestMatchHostGlob(t *testing.T) {
	tests := []struct {
		pattern  string
		hostname string
		want     bool
	}{
		{"github.com", "github.com", true},
		{"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com", false},
		{"*.googleapis.com", "storage.googleapis.com", true},
		{"*.googleapis.com", "deep.nested.googleapis.com", true},
		{"*.example.com", "example.com", false},
		{"example.com", "notexample.com", false},
		{"", "anything", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.pattern, tt.hostname), func(t *testing.T) {
			got := matchHostGlob(tt.pattern, tt.hostname)
			if got != tt.want {
				t.Errorf("matchHostGlob(%q, %q) = %v, want %v", tt.pattern, tt.hostname, got, tt.want)
			}
		})
	}
}

// --- hostOnly tests ---

func TestHostOnly(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com:443", "github.com"},
		{"github.com", "github.com"},
		{"[::1]:80", "::1"},
		{"127.0.0.1:8080", "127.0.0.1"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := hostOnly(tt.input)
			if got != tt.want {
				t.Errorf("hostOnly(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- isAllowedHost tests ---

func TestIsAllowedHost(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []string
		hostname string
		want     bool
	}{
		{"empty list allows all", nil, "anything.com", true},
		{"exact match", []string{"github.com"}, "github.com", true},
		{"exact no match", []string{"github.com"}, "gitlab.com", false},
		{"wildcard match", []string{"*.googleapis.com"}, "storage.googleapis.com", true},
		{"wildcard no match", []string{"*.googleapis.com"}, "github.com", false},
		{"multiple patterns", []string{"github.com", "*.googleapis.com"}, "storage.googleapis.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &AuthProxy{proxyConf: ProxyConfig{AllowedHosts: tt.allowed}}
			got := p.isAllowedHost(tt.hostname)
			if got != tt.want {
				t.Errorf("isAllowedHost(%q) = %v, want %v", tt.hostname, got, tt.want)
			}
		})
	}
}

// --- CA generation tests ---

func TestGenerateCA(t *testing.T) {
	ca, caPEM, err := generateCA("test-runner-12345678-abcd")
	if err != nil {
		t.Fatalf("generateCA failed: %v", err)
	}
	if ca == nil {
		t.Fatal("CA cert is nil")
	}
	if len(caPEM) == 0 {
		t.Fatal("CA PEM is empty")
	}

	// Verify PEM is valid.
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("failed to decode CA PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CA cert: %v", err)
	}
	if !cert.IsCA {
		t.Error("expected IsCA = true")
	}
	if !strings.Contains(cert.Subject.CommonName, "test-run") {
		t.Errorf("CommonName = %q, want to contain runner ID prefix", cert.Subject.CommonName)
	}
	if cert.NotAfter.Before(time.Now().Add(364 * 24 * time.Hour)) {
		t.Error("CA cert expires too soon")
	}
}

func TestGenerateCA_ShortRunnerID(t *testing.T) {
	// Runner ID shorter than 8 chars should not panic.
	ca, _, err := generateCA("short")
	if err != nil {
		t.Fatalf("generateCA failed: %v", err)
	}
	if ca == nil {
		t.Fatal("CA cert is nil")
	}
}

// --- certForHost tests ---

func TestCertForHost(t *testing.T) {
	ca, _, err := generateCA("test-runner")
	if err != nil {
		t.Fatalf("generateCA failed: %v", err)
	}

	p := &AuthProxy{bumpCA: ca}

	cert, err := p.certForHost("github.com")
	if err != nil {
		t.Fatalf("certForHost failed: %v", err)
	}

	// Parse and verify the generated cert.
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("failed to parse cert: %v", err)
	}
	if parsed.Subject.CommonName != "github.com" {
		t.Errorf("CommonName = %q, want %q", parsed.Subject.CommonName, "github.com")
	}
	if len(parsed.DNSNames) != 1 || parsed.DNSNames[0] != "github.com" {
		t.Errorf("DNSNames = %v, want [github.com]", parsed.DNSNames)
	}

	// Verify the cert chain has 2 entries (leaf + CA).
	if len(cert.Certificate) != 2 {
		t.Errorf("cert chain length = %d, want 2", len(cert.Certificate))
	}

	// Verify the cert is signed by the CA.
	caCert, _ := x509.ParseCertificate(ca.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = parsed.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Errorf("cert verification failed: %v", err)
	}
}

func TestCertForHost_Caching(t *testing.T) {
	ca, _, err := generateCA("test-runner")
	if err != nil {
		t.Fatalf("generateCA failed: %v", err)
	}

	p := &AuthProxy{bumpCA: ca}

	cert1, err := p.certForHost("example.com")
	if err != nil {
		t.Fatalf("certForHost (1) failed: %v", err)
	}
	cert2, err := p.certForHost("example.com")
	if err != nil {
		t.Fatalf("certForHost (2) failed: %v", err)
	}

	// Same pointer means it was cached.
	if cert1 != cert2 {
		t.Error("expected cached cert to be returned")
	}

	// Different hostname should get a different cert.
	cert3, err := p.certForHost("other.com")
	if err != nil {
		t.Fatalf("certForHost (3) failed: %v", err)
	}
	if cert1 == cert3 {
		t.Error("expected different cert for different hostname")
	}
}

// --- findMetadataHandler / hasDelegatedProviders ---

func TestFindMetadataHandler(t *testing.T) {
	t.Run("no metadata handler", func(t *testing.T) {
		p := &AuthProxy{providers: []CredentialProvider{&mockProvider{}}}
		if p.findMetadataHandler() != nil {
			t.Error("expected nil metadata handler")
		}
	})

	t.Run("with metadata handler", func(t *testing.T) {
		mp := &mockMetadataProvider{}
		p := &AuthProxy{providers: []CredentialProvider{&mockProvider{}, mp}}
		if p.findMetadataHandler() == nil {
			t.Error("expected non-nil metadata handler")
		}
	})
}

func TestHasDelegatedProviders(t *testing.T) {
	t.Run("no delegated", func(t *testing.T) {
		p := &AuthProxy{providers: []CredentialProvider{&mockProvider{}}}
		if p.hasDelegatedProviders() {
			t.Error("expected false")
		}
	})

	t.Run("with delegated", func(t *testing.T) {
		dp := &mockDelegatedProvider{}
		p := &AuthProxy{providers: []CredentialProvider{&mockProvider{}, dp}}
		if !p.hasDelegatedProviders() {
			t.Error("expected true")
		}
	})
}

// --- Proxy integration tests ---

func testLogger() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l.WithField("test", true)
}

func TestProxyStartStop(t *testing.T) {
	mp := &mockProvider{name: "test", hosts: []string{"example.com"}, token: "tok123"}

	p := &AuthProxy{
		runnerID:  "test-runner",
		providers: []CredentialProvider{mp},
		proxyConf: ProxyConfig{ListenPort: 0, SSLBump: true},
		logger:    testLogger(),
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}

	// Generate CA.
	ca, caPEM, err := generateCA("test-runner")
	if err != nil {
		t.Fatalf("generateCA failed: %v", err)
	}
	p.bumpCA = ca
	p.CACertPEM = caPEM

	// Use localhost for binding (no netns).
	p.gatewayIP = "127.0.0.1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.ctx, p.cancel = context.WithCancel(ctx)

	// Bind proxy listener manually (avoids port conflicts).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	p.proxyLn = ln
	p.wg.Add(1)
	go p.serveProxy()

	// Stop should not hang.
	p.Stop()

	if !mp.stopped {
		t.Error("expected provider Stop() to be called")
	}
}

func TestTokenUpdateEndpoint(t *testing.T) {
	dp := &mockDelegatedProvider{
		mockProvider: mockProvider{name: "delegated", hosts: []string{"github.com"}},
	}

	p := &AuthProxy{
		runnerID:  "test-runner",
		providers: []CredentialProvider{dp},
		proxyConf: ProxyConfig{},
		logger:    testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.ctx, p.cancel = context.WithCancel(ctx)

	// Start token update server on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	p.updateLn = ln
	p.wg.Add(1)
	go p.serveTokenUpdate()
	defer p.Stop()

	addr := ln.Addr().String()

	t.Run("POST with valid token", func(t *testing.T) {
		body := `{"provider":"delegated","token":"ghp_abc123","expires_at":"2030-01-01T00:00:00Z"}`
		resp, err := http.Post("http://"+addr+"/update-token", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200, body: %s", resp.StatusCode, b)
		}
		if dp.pushedToken != "ghp_abc123" {
			t.Errorf("pushed token = %q, want %q", dp.pushedToken, "ghp_abc123")
		}
		want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		if !dp.pushedExpiresAt.Equal(want) {
			t.Errorf("pushed expiresAt = %v, want %v", dp.pushedExpiresAt, want)
		}
	})

	t.Run("GET method not allowed", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/update-token")
		if err != nil {
			t.Fatalf("GET failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		body := `{"provider":"unknown","token":"tok"}`
		resp, err := http.Post("http://"+addr+"/update-token", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		resp, err := http.Post("http://"+addr+"/update-token", "application/json", strings.NewReader("not json"))
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("provider without TokenReceiver", func(t *testing.T) {
		// Add a non-delegated provider.
		p.providers = append(p.providers, &mockProvider{name: "plain"})
		body := `{"provider":"plain","token":"tok"}`
		resp, err := http.Post("http://"+addr+"/update-token", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestMetadataServer_RequiresHeader(t *testing.T) {
	mp := &mockMetadataProvider{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "Missing Metadata-Flavor header", http.StatusForbidden)
			return
		}
		mp.ServeMetadata(w, r)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Run("without header", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/computeMetadata/v1/instance/service-accounts/default/token")
		if err != nil {
			t.Fatalf("GET failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("with header", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if len(mp.servedPaths) != 1 {
			t.Errorf("served paths = %v, want 1 path", mp.servedPaths)
		}
	})
}

// TestSSLBumpProxy tests the full CONNECT → TLS → credential injection flow.
func TestSSLBumpProxy(t *testing.T) {
	// 1. Start a fake upstream HTTPS server that echoes the Authorization header.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "auth=%s", r.Header.Get("Authorization"))
	}))
	defer upstream.Close()

	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	// 2. Build the auth proxy with a mock provider that injects credentials.
	ca, caPEM, err := generateCA("test-ssl-bump")
	if err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	mp := &mockProvider{
		name:  "test",
		hosts: []string{hostOnly(upstreamHost)},
		token: "Bearer secret-token-123",
	}

	// Create a transport that trusts the upstream test server.
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.TLS.Certificates[0].Leaf)
	// We need to add the upstream's cert - but httptest uses a self-signed cert.
	// Use InsecureSkipVerify for the upstream connection in tests.
	proxyTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test only
	}

	p := &AuthProxy{
		runnerID:  "test-runner",
		providers: []CredentialProvider{mp},
		proxyConf: ProxyConfig{SSLBump: true},
		bumpCA:    ca,
		CACertPEM: caPEM,
		transport: proxyTransport,
		logger:    testLogger(),
	}

	// 3. Start proxy listener.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.ctx, p.cancel = context.WithCancel(ctx)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.proxyLn = proxyLn
	p.wg.Add(1)
	go p.serveProxy()
	defer p.Stop()

	proxyAddr := proxyLn.Addr().String()

	// 4. Connect through the proxy using CONNECT method.
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// Send CONNECT.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamHost, upstreamHost)

	// Read 200 Connection Established.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// 5. TLS handshake with proxy's MITM cert (trust our CA).
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	tlsConn := tls.Client(conn, &tls.Config{
		RootCAs:    caPool,
		ServerName: hostOnly(upstreamHost),
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	defer tlsConn.Close()

	// 6. Send HTTP request through the MITM'd connection.
	req, _ := http.NewRequest("GET", "https://"+upstreamHost+"/test", nil)
	if err := req.Write(tlsConn); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// 7. Read response.
	resp, err = http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Verify the proxy injected the credentials.
	if !strings.Contains(bodyStr, "secret-token-123") {
		t.Errorf("response body = %q, want to contain injected token", bodyStr)
	}
}

// TestProxyConnectBlockedHost verifies that CONNECT to a non-allowed host is rejected.
func TestProxyConnectBlockedHost(t *testing.T) {
	ca, caPEM, err := generateCA("test")
	if err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	p := &AuthProxy{
		runnerID:  "test",
		providers: nil,
		proxyConf: ProxyConfig{
			SSLBump:      true,
			AllowedHosts: []string{"allowed.com"},
		},
		bumpCA:    ca,
		CACertPEM: caPEM,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
		logger:    testLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.ctx, p.cancel = context.WithCancel(ctx)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.proxyLn = proxyLn
	p.wg.Add(1)
	go p.serveProxy()
	defer p.Stop()

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT blocked.com:443 HTTP/1.1\r\nHost: blocked.com:443\r\n\r\n")

	var buf bytes.Buffer
	io.Copy(&buf, conn)
	if !strings.Contains(buf.String(), "403") {
		t.Errorf("response = %q, want to contain 403", buf.String())
	}
}

// TestNewAuthProxy_DefaultListenPort verifies default port is set when not configured.
func TestNewAuthProxy_DefaultListenPort(t *testing.T) {
	RegisterProvider("test-np", func(_ ProviderConfig) (CredentialProvider, error) {
		return &mockProvider{name: "test-np"}, nil
	})
	defer delete(registry, "test-np")

	proxy, err := NewAuthProxy("runner-1", AuthConfig{
		Providers: []ProviderConfig{{Type: "test-np"}},
		Proxy:     ProxyConfig{}, // ListenPort = 0
	}, "", "127.0.0.1", "", testLogger())
	if err != nil {
		t.Fatalf("NewAuthProxy failed: %v", err)
	}
	if proxy.proxyConf.ListenPort != 3128 {
		t.Errorf("ListenPort = %d, want 3128", proxy.proxyConf.ListenPort)
	}
	if proxy.CACertPEM == nil {
		t.Error("CACertPEM should not be nil")
	}
}
