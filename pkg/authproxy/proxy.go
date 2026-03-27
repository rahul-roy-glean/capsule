package authproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
)

const (
	// tokenUpdatePort is the port for the host-side token update endpoint.
	tokenUpdatePort = 9443
	// metadata port for GCP metadata emulation.
	metadataPort = 80
)

// AuthProxy provides per-VM credential injection via an HTTPS proxy with
// SSL bump and an optional GCP metadata emulation endpoint.
type AuthProxy struct {
	runnerID  string
	providers []CredentialProvider
	proxyConf ProxyConfig

	// Listeners bound inside the VM's network namespace.
	metadataLn net.Listener // gatewayIP:80 (GCP metadata)
	proxyLn    net.Listener // gatewayIP:listenPort (HTTPS proxy)

	// Token update listener on host veth (not reachable from VM).
	updateLn net.Listener

	// Per-VM TLS CA for SSL bump.
	bumpCA    *tls.Certificate
	CACertPEM []byte // PEM-encoded CA cert for injection into VM trust store
	certCache sync.Map

	// Network info.
	nsPath     string // network namespace path (empty = no namespace)
	gatewayIP  string // e.g., "172.16.0.1"
	hostVethIP string // e.g., "10.200.{slot}.1"

	// HTTP transport for upstream connections (reused across requests).
	transport *http.Transport

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *logrus.Entry
}

// NewAuthProxy creates an auth proxy for a single VM.
// nsPath is the path to the VM's network namespace (e.g., /var/run/netns/fc-xxx).
// gatewayIP is the inner bridge gateway (172.16.0.1).
// hostVethIP is the host-side veth IP (10.200.{slot}.1) for the token update endpoint.
func NewAuthProxy(runnerID string, config AuthConfig, nsPath, gatewayIP, hostVethIP string, logger *logrus.Entry) (*AuthProxy, error) {
	providers, err := BuildProviders(config.Providers)
	if err != nil {
		return nil, fmt.Errorf("building providers: %w", err)
	}

	ca, caPEM, err := generateCA(runnerID)
	if err != nil {
		return nil, fmt.Errorf("generating CA: %w", err)
	}

	proxyConf := config.Proxy
	if proxyConf.ListenPort == 0 {
		proxyConf.ListenPort = 3128
	}
	// Default SSLBump to true when providers exist. Without SSL bump the proxy
	// just tunnels bytes and cannot inject credentials, making it useless.
	if !proxyConf.SSLBump && len(providers) > 0 {
		proxyConf.SSLBump = true
	}

	return &AuthProxy{
		runnerID:   runnerID,
		providers:  providers,
		proxyConf:  proxyConf,
		bumpCA:     ca,
		CACertPEM:  caPEM,
		nsPath:     nsPath,
		gatewayIP:  gatewayIP,
		hostVethIP: hostVethIP,
		transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		logger: logger.WithField("component", "auth-proxy"),
	}, nil
}

// Start binds listeners and begins serving. Call Stop() to tear down.
func (p *AuthProxy) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	// Start all providers (background refresh goroutines).
	for _, prov := range p.providers {
		if err := prov.Start(p.ctx); err != nil {
			return fmt.Errorf("starting provider %s: %w", prov.Name(), err)
		}
	}

	// Bind the token update endpoint BEFORE any listenInNamespace calls.
	// listenInNamespace switches the OS thread's network namespace; any net.Listen
	// called after that (even on a different goroutine) may land on a thread whose
	// netns was not fully restored. Binding here, before any namespace switching,
	// guarantees the socket is created in the host namespace.
	if p.hasDelegatedProviders() && p.hostVethIP != "" {
		updateAddr := fmt.Sprintf("%s:%d", p.hostVethIP, tokenUpdatePort)
		uln, err := listenInHostNamespace(updateAddr)
		if err != nil {
			return fmt.Errorf("binding token update listener on %s: %w", updateAddr, err)
		}
		p.updateLn = uln
		p.wg.Add(1)
		go p.serveTokenUpdate()
	}

	// Start metadata server if any provider implements MetadataHandler.
	if handler := p.findMetadataHandler(); handler != nil {
		ln, err := p.listenInNamespace(fmt.Sprintf("%s:%d", p.gatewayIP, metadataPort))
		if err != nil {
			return fmt.Errorf("binding metadata listener: %w", err)
		}
		p.metadataLn = ln
		p.wg.Add(1)
		go p.serveMetadata(handler)
	}

	// Start HTTPS proxy.
	proxyAddr := fmt.Sprintf("%s:%d", p.gatewayIP, p.proxyConf.ListenPort)
	ln, err := p.listenInNamespace(proxyAddr)
	if err != nil {
		return fmt.Errorf("binding proxy listener on %s: %w", proxyAddr, err)
	}
	p.proxyLn = ln
	p.wg.Add(1)
	go p.serveProxy()

	p.logger.WithFields(logrus.Fields{
		"runner_id":    p.runnerID,
		"proxy_addr":   proxyAddr,
		"providers":    len(p.providers),
		"ssl_bump":     p.proxyConf.SSLBump,
		"has_metadata": p.findMetadataHandler() != nil,
	}).Info("Auth proxy started")

	return nil
}

// Stop gracefully shuts down the proxy and all providers.
func (p *AuthProxy) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.metadataLn != nil {
		p.metadataLn.Close()
	}
	if p.proxyLn != nil {
		p.proxyLn.Close()
	}
	if p.updateLn != nil {
		p.updateLn.Close()
	}
	for _, prov := range p.providers {
		prov.Stop()
	}
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	p.wg.Wait()
	p.logger.WithField("runner_id", p.runnerID).Info("Auth proxy stopped")
}

// TokenUpdateAddr returns the host-side address for pushing tokens to delegated providers.
// Returns empty string if no token update endpoint is running.
func (p *AuthProxy) TokenUpdateAddr() string {
	if p.updateLn != nil {
		return p.updateLn.Addr().String()
	}
	return ""
}

// listenInHostNamespace creates a TCP listener in the host network namespace.
// It explicitly pins the OS thread and sets the host netns to counteract any
// thread that was previously used for a namespace switch and may not have been
// fully restored to the host namespace by the Go scheduler.
func listenInHostNamespace(addr string) (net.Listener, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Open the init process's network namespace — this is always the true host
	// namespace. We cannot use netns.Get() because the current thread may have
	// been left in a VM namespace by a previous listenInNamespace call (Go
	// reuses OS threads across goroutines).
	hostNS, err := netns.GetFromPath("/proc/1/ns/net")
	if err != nil {
		return nil, fmt.Errorf("opening host namespace /proc/1/ns/net: %w", err)
	}
	defer hostNS.Close()

	if err := netns.Set(hostNS); err != nil {
		return nil, fmt.Errorf("setting host namespace: %w", err)
	}

	return net.Listen("tcp", addr)
}

func (p *AuthProxy) listenInNamespace(addr string) (net.Listener, error) {
	if p.nsPath == "" {
		return net.Listen("tcp", addr)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("getting current namespace: %w", err)
	}
	defer origNS.Close()

	targetNS, err := netns.GetFromPath(p.nsPath)
	if err != nil {
		return nil, fmt.Errorf("opening namespace %s: %w", p.nsPath, err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return nil, fmt.Errorf("entering namespace: %w", err)
	}
	defer netns.Set(origNS) //nolint:errcheck

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s in namespace: %w", addr, err)
	}
	return ln, nil
}

// serveMetadata runs the metadata HTTP server (port 80 inside netns).
func (p *AuthProxy) serveMetadata(handler MetadataHandler) {
	defer p.wg.Done()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// GCP metadata requires this header.
			if r.Header.Get("Metadata-Flavor") != "Google" {
				http.Error(w, "Missing Metadata-Flavor header", http.StatusForbidden)
				return
			}
			handler.ServeMetadata(w, r)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		<-p.ctx.Done()
		srv.Close()
	}()

	if err := srv.Serve(p.metadataLn); err != nil && p.ctx.Err() == nil {
		p.logger.WithError(err).Error("Metadata server exited with error")
	}
}

// serveProxy runs the HTTPS CONNECT proxy (port 3128 inside netns).
func (p *AuthProxy) serveProxy() {
	defer p.wg.Done()

	for {
		conn, err := p.proxyLn.Accept()
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			p.logger.WithError(err).Debug("Proxy accept error")
			continue
		}
		go p.handleProxyConn(conn)
	}
}

// handleProxyConn handles a single proxy connection.
func (p *AuthProxy) handleProxyConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	if req.Method == http.MethodConnect {
		p.handleConnect(conn, req)
	} else {
		p.handleHTTP(conn, req)
	}
}

// handleConnect handles CONNECT requests with optional SSL bump.
func (p *AuthProxy) handleConnect(clientConn net.Conn, connectReq *http.Request) {
	targetHost := connectReq.Host
	hostname := hostOnly(targetHost)

	if !p.isAllowedHost(hostname) {
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			ProtoMajor: 1, ProtoMinor: 1,
			Body: io.NopCloser(strings.NewReader("host not allowed by proxy policy")),
		}
		resp.Write(clientConn)
		return
	}

	// Send 200 Connection Established.
	fmt.Fprint(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	if !p.proxyConf.SSLBump {
		p.tunnel(clientConn, targetHost)
		return
	}

	// Only MITM hosts that have a matching credential provider.
	// For all other hosts, tunnel bytes directly — avoids breaking
	// clients with their own CA stores (e.g., Java/Bazel).
	hasProvider := false
	for _, prov := range p.providers {
		if prov.Matches(hostname) {
			hasProvider = true
			break
		}
	}
	if !hasProvider {
		p.tunnel(clientConn, targetHost)
		return
	}

	// SSL bump: MITM the connection to read/modify HTTP requests.
	cert, err := p.certForHost(hostname)
	if err != nil {
		p.logger.WithError(err).WithField("host", hostname).Warn("Failed to generate cert for host")
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		p.logger.WithError(err).WithField("host", hostname).Debug("TLS handshake with client failed")
		return
	}
	defer tlsConn.Close()

	// Read requests from the MITM'd connection and forward them.
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return // connection closed or error
		}

		req.URL.Scheme = "https"
		req.URL.Host = targetHost
		req.RequestURI = ""

		// Inject credentials from matching provider.
		for _, prov := range p.providers {
			if prov.Matches(hostname) {
				if injErr := prov.InjectCredentials(req); injErr != nil {
					p.logger.WithError(injErr).WithField("provider", prov.Name()).Warn("Credential injection failed")
				}
				break
			}
		}

		resp, err := p.transport.RoundTrip(req)
		if err != nil {
			errResp := &http.Response{
				StatusCode: http.StatusBadGateway,
				ProtoMajor: 1, ProtoMinor: 1,
				Body: io.NopCloser(strings.NewReader(err.Error())),
			}
			errResp.Write(tlsConn)
			return
		}
		if err := resp.Write(tlsConn); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
	}
}

// handleHTTP handles non-CONNECT (plain HTTP) proxy requests.
func (p *AuthProxy) handleHTTP(clientConn net.Conn, req *http.Request) {
	hostname := hostOnly(req.Host)

	if !p.isAllowedHost(hostname) {
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			ProtoMajor: 1, ProtoMinor: 1,
			Body: io.NopCloser(strings.NewReader("host not allowed")),
		}
		resp.Write(clientConn)
		return
	}

	req.RequestURI = ""

	for _, prov := range p.providers {
		if prov.Matches(hostname) {
			if err := prov.InjectCredentials(req); err != nil {
				p.logger.WithError(err).WithField("provider", prov.Name()).Warn("Credential injection failed")
			}
			break
		}
	}

	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		errResp := &http.Response{
			StatusCode: http.StatusBadGateway,
			ProtoMajor: 1, ProtoMinor: 1,
			Body: io.NopCloser(strings.NewReader(err.Error())),
		}
		errResp.Write(clientConn)
		return
	}
	defer resp.Body.Close()
	resp.Write(clientConn)
}

// ProxyAddress returns the proxy listen address (e.g., "172.16.0.1:3128").
func (p *AuthProxy) ProxyAddress() string {
	return fmt.Sprintf("%s:%d", p.gatewayIP, p.proxyConf.ListenPort)
}

// GatewayIP returns the gateway IP used by this proxy.
func (p *AuthProxy) GatewayIP() string {
	return p.gatewayIP
}

// tunnel copies bytes bidirectionally without decryption (no SSL bump).
func (p *AuthProxy) tunnel(clientConn net.Conn, targetHost string) {
	upstream, err := net.DialTimeout("tcp", targetHost, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}
	go cp(upstream, clientConn)
	go cp(clientConn, upstream)
	wg.Wait()
}

// serveTokenUpdate runs the host-side token push endpoint.
func (p *AuthProxy) serveTokenUpdate() {
	defer p.wg.Done()

	mux := http.NewServeMux()
	mux.HandleFunc("/update-token", p.handleUpdateToken)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		<-p.ctx.Done()
		srv.Close()
	}()

	if err := srv.Serve(p.updateLn); err != nil && p.ctx.Err() == nil {
		p.logger.WithError(err).Error("Token update server exited with error")
	}
}

type tokenUpdateRequest struct {
	Provider  string    `json:"provider"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	TokenType string    `json:"token_type,omitempty"` // for providers accepting multiple token types (e.g., "user", "bot")
}

func (p *AuthProxy) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req tokenUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	for _, prov := range p.providers {
		if prov.Name() == req.Provider {
			// Try typed token receiver first (supports multiple token types).
			if req.TokenType != "" {
				if typed, ok := prov.(TypedTokenReceiver); ok {
					if err := typed.UpdateTypedToken(req.TokenType, req.Token, req.ExpiresAt); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			// Fall back to simple token receiver.
			if receiver, ok := prov.(TokenReceiver); ok {
				if err := receiver.UpdateToken(req.Token, req.ExpiresAt); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "provider does not accept token updates", http.StatusBadRequest)
			return
		}
	}
	http.Error(w, "provider not found", http.StatusNotFound)
}

// certForHost returns (or generates and caches) a TLS certificate for the given
// hostname, signed by the per-VM CA.
func (p *AuthProxy) certForHost(hostname string) (*tls.Certificate, error) {
	if cached, ok := p.certCache.Load(hostname); ok {
		return cached.(*tls.Certificate), nil
	}

	caCert, err := x509.ParseCertificate(p.bumpCA.Certificate[0])
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, p.bumpCA.PrivateKey)
	if err != nil {
		return nil, err
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{certDER, p.bumpCA.Certificate[0]},
		PrivateKey:  key,
	}
	p.certCache.Store(hostname, cert)
	return cert, nil
}

// generateCA creates a self-signed ECDSA CA certificate for SSL bump.
func generateCA(runnerID string) (*tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generating serial: %w", err)
	}

	shortID := runnerID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("Firecracker Auth Proxy CA (%s)", shortID),
			Organization: []string{"Firecracker Auth Proxy"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA keypair: %w", err)
	}

	return &tlsCert, certPEM, nil
}

// isAllowedHost checks whether a hostname is in the allowed list.
// An empty allowed list permits all hosts.
func (p *AuthProxy) isAllowedHost(hostname string) bool {
	if len(p.proxyConf.AllowedHosts) == 0 {
		return true
	}
	for _, pattern := range p.proxyConf.AllowedHosts {
		if matchHostGlob(pattern, hostname) {
			return true
		}
	}
	return false
}

// matchHostGlob matches a hostname against a glob pattern.
// Supports *.example.com (matches foo.example.com and bar.foo.example.com).
func matchHostGlob(pattern, hostname string) bool {
	if pattern == hostname {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(hostname, suffix)
	}
	return false
}

// hostOnly strips the port from a host:port string.
func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport // no port
	}
	return host
}

func (p *AuthProxy) findMetadataHandler() MetadataHandler {
	for _, prov := range p.providers {
		if mh, ok := prov.(MetadataHandler); ok {
			return mh
		}
	}
	return nil
}

func (p *AuthProxy) hasDelegatedProviders() bool {
	for _, prov := range p.providers {
		if _, ok := prov.(TokenReceiver); ok {
			return true
		}
	}
	return false
}
