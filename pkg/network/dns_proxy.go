//go:build linux
// +build linux

package network

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// DNSProxy is an in-process DNS proxy that intercepts guest DNS queries,
// enforces domain allow/block lists, and populates ipsets with resolved IPs.
// One proxy runs per VM that needs domain filtering.
type DNSProxy struct {
	config   DNSProxyConfig
	server   *dns.Server
	client   *dns.Client
	mu       sync.RWMutex
	allowed  map[string]bool // lowercase domain -> allowed (wildcard-expanded on query)
	blocked  map[string]bool // lowercase domain -> blocked
	logger   *logrus.Entry
	running  atomic.Bool
	stopCh   chan struct{}
	ipCount  atomic.Int64 // approximate total IPs in ipset
}

// DNSProxyConfig configures the DNS proxy.
type DNSProxyConfig struct {
	ListenAddr      string
	UpstreamServers []string
	AllowedDomains  []string
	BlockedDomains  []string
	DomIPSetName    string
	NSName          string
	MaxIPsPerDomain int
	MaxIPSetEntries int
	VMID            string
	Logger          *logrus.Logger
}

// NewDNSProxy creates a new DNS proxy (does not start it).
func NewDNSProxy(cfg DNSProxyConfig) *DNSProxy {
	if len(cfg.UpstreamServers) == 0 {
		cfg.UpstreamServers = []string{"8.8.8.8:53"}
	} else {
		// Ensure servers have port
		for i, s := range cfg.UpstreamServers {
			if _, _, err := net.SplitHostPort(s); err != nil {
				cfg.UpstreamServers[i] = s + ":53"
			}
		}
	}
	if cfg.MaxIPsPerDomain <= 0 {
		cfg.MaxIPsPerDomain = 64
	}
	if cfg.MaxIPSetEntries <= 0 {
		cfg.MaxIPSetEntries = 10000
	}

	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}

	allowed := make(map[string]bool)
	for _, d := range cfg.AllowedDomains {
		allowed[strings.ToLower(d)] = true
	}
	blocked := make(map[string]bool)
	for _, d := range cfg.BlockedDomains {
		blocked[strings.ToLower(d)] = true
	}

	return &DNSProxy{
		config:  cfg,
		client:  &dns.Client{Timeout: 5 * time.Second},
		allowed: allowed,
		blocked: blocked,
		logger: logger.WithFields(logrus.Fields{
			"component": "dns-proxy",
			"vm_id":     cfg.VMID,
		}),
		stopCh: make(chan struct{}),
	}
}

// Start starts the DNS proxy listener. runInNS executes a function inside the
// VM's network namespace (the proxy binds in the namespace context).
func (p *DNSProxy) Start(runInNS func(func() error) error) error {
	mux := dns.NewServeMux()
	mux.HandleFunc(".", p.handleQuery)

	p.server = &dns.Server{
		Addr:    p.config.ListenAddr,
		Net:     "udp",
		Handler: mux,
	}

	// Start the server inside the namespace
	errCh := make(chan error, 1)
	go func() {
		err := runInNS(func() error {
			return p.server.ListenAndServe()
		})
		errCh <- err
	}()

	// Give it a moment to bind
	select {
	case err := <-errCh:
		return fmt.Errorf("dns proxy failed to start: %w", err)
	case <-time.After(100 * time.Millisecond):
		// Likely started successfully
	}

	p.running.Store(true)

	// Health monitoring goroutine
	go p.healthMonitor()

	p.logger.WithField("listen", p.config.ListenAddr).Info("DNS proxy started")
	return nil
}

// Stop shuts down the DNS proxy.
func (p *DNSProxy) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}
	close(p.stopCh)
	if p.server != nil {
		p.server.Shutdown()
	}
	p.logger.Info("DNS proxy stopped")
}

// UpdateDomains atomically updates the allowed/blocked domain lists.
func (p *DNSProxy) UpdateDomains(allowed, blocked []string) {
	newAllowed := make(map[string]bool)
	for _, d := range allowed {
		newAllowed[strings.ToLower(d)] = true
	}
	newBlocked := make(map[string]bool)
	for _, d := range blocked {
		newBlocked[strings.ToLower(d)] = true
	}

	p.mu.Lock()
	p.allowed = newAllowed
	p.blocked = newBlocked
	p.mu.Unlock()
}

// handleQuery processes a DNS query.
func (p *DNSProxy) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		dns.HandleFailed(w, r)
		return
	}

	qname := strings.ToLower(strings.TrimSuffix(r.Question[0].Name, "."))

	// Check domain against allow/block lists
	action := p.checkDomain(qname)
	if action == "block" {
		p.logger.WithField("domain", qname).Debug("DNS query blocked")
		resp := &dns.Msg{}
		resp.SetRcode(r, dns.RcodeNameError) // NXDOMAIN
		w.WriteMsg(resp)
		return
	}

	// Forward to upstream
	resp, _, err := p.client.Exchange(r, p.config.UpstreamServers[0])
	if err != nil {
		p.logger.WithError(err).WithField("domain", qname).Warn("Upstream DNS query failed")
		dns.HandleFailed(w, r)
		return
	}

	// Extract IPs and add to ipset
	if action == "allow" {
		p.addResponseIPsToIPSet(qname, resp)
	}

	w.WriteMsg(resp)
}

// checkDomain checks if a domain is allowed or blocked.
// Returns "allow", "block", or "passthrough".
func (p *DNSProxy) checkDomain(domain string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Check blocked first
	if p.matchDomain(domain, p.blocked) {
		return "block"
	}

	// If there's an allow list, domain must match it
	if len(p.allowed) > 0 {
		if p.matchDomain(domain, p.allowed) {
			return "allow"
		}
		return "block" // not in allowlist = blocked
	}

	// No allow list = allow everything (that isn't blocked)
	return "allow"
}

// matchDomain checks if a domain matches any entry in the map, supporting wildcards.
// Wildcard matching: "*.example.com" matches "foo.example.com" and "bar.foo.example.com".
func (p *DNSProxy) matchDomain(domain string, domainMap map[string]bool) bool {
	if domainMap[domain] {
		return true
	}

	// Check wildcard matches by walking up the domain hierarchy
	parts := strings.Split(domain, ".")
	for i := 1; i < len(parts); i++ {
		wildcard := "*." + strings.Join(parts[i:], ".")
		if domainMap[wildcard] {
			return true
		}
	}
	return false
}

// addResponseIPsToIPSet extracts A/AAAA records and adds IPs to the domain ipset.
func (p *DNSProxy) addResponseIPsToIPSet(domain string, resp *dns.Msg) {
	var ips []net.IP
	var minTTL uint32 = 300

	for _, rr := range resp.Answer {
		switch r := rr.(type) {
		case *dns.A:
			ips = append(ips, r.A)
			if r.Hdr.Ttl < minTTL {
				minTTL = r.Hdr.Ttl
			}
		case *dns.AAAA:
			ips = append(ips, r.AAAA)
			if r.Hdr.Ttl < minTTL {
				minTTL = r.Hdr.Ttl
			}
		}
	}

	if len(ips) == 0 {
		return
	}

	// Cap TTL
	if minTTL > 300 {
		minTTL = 300
	}
	if minTTL < 10 {
		minTTL = 10
	}

	// Enforce per-domain cap
	if len(ips) > p.config.MaxIPsPerDomain {
		ips = ips[:p.config.MaxIPsPerDomain]
	}

	timeoutStr := fmt.Sprintf("%d", minTTL)

	for _, ip := range ips {
		// Check total ipset capacity
		if int(p.ipCount.Load()) >= p.config.MaxIPSetEntries {
			p.logger.WithFields(logrus.Fields{
				"domain": domain,
				"vm_id":  p.config.VMID,
			}).Warn("ipset full, cannot add IP (policy_ipset_full)")
			break
		}

		// Add to ipset with timeout (inside namespace)
		err := exec.Command("ip", "netns", "exec", p.config.NSName,
			"ipset", "add", p.config.DomIPSetName, ip.String(),
			"timeout", timeoutStr, "-exist").Run()
		if err != nil {
			p.logger.WithError(err).WithFields(logrus.Fields{
				"ip":     ip.String(),
				"domain": domain,
			}).Warn("Failed to add IP to domain ipset")
		} else {
			p.ipCount.Add(1)
		}
	}
}

// healthMonitor periodically checks if the proxy is still running.
func (p *DNSProxy) healthMonitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if !p.running.Load() {
				p.logger.Warn("DNS proxy not running (dnsproxy_up=0)")
			}
		}
	}
}
