// Package domaincheck verifies that an expected domain points at this host
// with DNS-only (non-CDN) records and that TLS answers on the public listen.
// When dnsListen is set it also asserts the UDP carrier port is bound.
// It is observational only: it never updates the control-plane catalog.
package domaincheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Result is a single check outcome.
type Result struct {
	Name    string
	OK      bool
	Detail  string
	Skipped bool
}

// Report aggregates domain health for doctor/TUI/status.
type Report struct {
	Domain  string
	Results []Result
}

// Failed reports whether any non-skipped check failed.
func (r Report) Failed() bool {
	for _, x := range r.Results {
		if !x.Skipped && !x.OK {
			return true
		}
	}
	return false
}

// Check resolves domain, rejects known CDN edges, compares A/AAAA to this host,
// dials TCP on listen, and attempts a TLS ClientHello with ServerName=domain.
// When dnsListen is non-empty (e.g. ":53"), also asserts that UDP port is listening.
func Check(ctx context.Context, domain, listen, dnsListen string) Report {
	domain = strings.TrimSpace(strings.ToLower(domain))
	rep := Report{Domain: domain}
	if domain == "" {
		rep.Results = append(rep.Results, Result{Name: "domain", Skipped: true, Detail: "domain not set"})
		return rep
	}
	if !validHostname(domain) {
		rep.Results = append(rep.Results, Result{Name: "domain", OK: false, Detail: "invalid hostname"})
		return rep
	}

	ips, err := lookupIPs(ctx, domain)
	if err != nil || len(ips) == 0 {
		detail := "does not resolve"
		if err != nil {
			detail = err.Error()
		}
		rep.Results = append(rep.Results, Result{Name: "dns", OK: false, Detail: detail})
		return rep
	}
	var cdn []string
	var resolved []string
	for _, ip := range ips {
		s := ip.String()
		resolved = append(resolved, s)
		if IsCDN(ip) {
			cdn = append(cdn, s)
		}
	}
	if len(cdn) > 0 {
		rep.Results = append(rep.Results, Result{
			Name: "dns", OK: false,
			Detail: fmt.Sprintf("%s → CDN edge %s (use DNS-only / grey-cloud A/AAAA to this host)", domain, strings.Join(cdn, ",")),
		})
		return rep
	}

	local := localIPs()
	match := false
	for _, ip := range ips {
		for _, lip := range local {
			if ip.Equal(lip) {
				match = true
				break
			}
		}
	}
	if len(local) == 0 {
		rep.Results = append(rep.Results, Result{
			Name: "dns", OK: true,
			Detail: fmt.Sprintf("%s → %s (could not detect local IPs to compare)", domain, strings.Join(resolved, ",")),
		})
	} else if !match {
		rep.Results = append(rep.Results, Result{
			Name: "dns", OK: false,
			Detail: fmt.Sprintf("%s → %s but this host is %s", domain, strings.Join(resolved, ","), joinIPs(local)),
		})
	} else {
		rep.Results = append(rep.Results, Result{
			Name: "dns", OK: true,
			Detail: fmt.Sprintf("%s → this host (%s)", domain, strings.Join(resolved, ",")),
		})
	}

	hostPort := publicDialAddr(listen)
	if hostPort == "" {
		rep.Results = append(rep.Results, Result{Name: "tcp", Skipped: true, Detail: "listen address empty"})
		return rep
	}
	// Prefer dialing a resolved IP:port so we test this host, not a stale cache via hostname.
	dialTarget := hostPort
	if h, p, err := net.SplitHostPort(hostPort); err == nil && (h == "" || h == "0.0.0.0" || h == "::") {
		if len(ips) > 0 {
			dialTarget = net.JoinHostPort(ips[0].String(), p)
		}
	}

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", dialTarget)
	if err != nil {
		rep.Results = append(rep.Results, Result{Name: "tcp", OK: false, Detail: dialTarget + ": " + err.Error()})
		return rep
	}
	rep.Results = append(rep.Results, Result{Name: "tcp", OK: true, Detail: dialTarget + " accepts TCP"})

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: domain,
		MinVersion: tls.VersionTLS12,
	})
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsConn.HandshakeContext(dctx); err != nil {
		_ = tlsConn.Close()
		rep.Results = append(rep.Results, Result{Name: "tls", OK: false, Detail: "SNI " + domain + ": " + err.Error()})
		return rep
	}
	state := tlsConn.ConnectionState()
	_ = tlsConn.Close()
	cn := ""
	if len(state.PeerCertificates) > 0 {
		cn = state.PeerCertificates[0].Subject.CommonName
	}
	rep.Results = append(rep.Results, Result{
		Name: "tls", OK: true,
		Detail: fmt.Sprintf("SNI %s handshake ok (peer CN=%q)", domain, cn),
	})

	if dnsListen = strings.TrimSpace(dnsListen); dnsListen != "" {
		rep.Results = append(rep.Results, checkUDPListen(dnsListen))
	}
	return rep
}

func checkUDPListen(dnsListen string) Result {
	port := "53"
	if _, p, err := net.SplitHostPort(dnsListen); err == nil && p != "" {
		port = p
	} else if strings.HasPrefix(dnsListen, ":") {
		if p := strings.TrimPrefix(dnsListen, ":"); p != "" {
			port = p
		}
	}
	if udpLocalListening(port) {
		return Result{Name: "udp", OK: true, Detail: "listening on " + dnsListen + " (carrier zone = domain)"}
	}
	return Result{Name: "udp", OK: false, Detail: dnsListen + " not listening"}
}

func udpLocalListening(port string) bool {
	out, err := exec.Command("ss", "-uln").Output()
	if err != nil {
		return false
	}
	needle := ":" + port
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 || strings.Contains(h, "..") {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

func lookupIPs(ctx context.Context, host string) ([]net.IP, error) {
	r := net.DefaultResolver
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

func localIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return
		}
		s := ip.String()
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, ip)
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				add(v.IP)
			case *net.IPAddr:
				add(v.IP)
			}
		}
	}
	return out
}

func joinIPs(ips []net.IP) string {
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		parts = append(parts, ip.String())
	}
	return strings.Join(parts, ",")
}

func publicDialAddr(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	if h, p, err := net.SplitHostPort(listen); err == nil {
		if h == "" || h == "0.0.0.0" || h == "::" {
			return net.JoinHostPort("127.0.0.1", p)
		}
		return listen
	}
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return net.JoinHostPort(listen, "443")
}

// IsCDN reports whether ip looks like a common Cloudflare edge prefix.
// Proxied (orange-cloud) DNS breaks the relay knock.
func IsCDN(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	a, b := ip4[0], ip4[1]
	switch {
	case a == 104 && b >= 16 && b <= 28:
		return true
	case a == 172 && b >= 64 && b <= 67:
		return true
	case a == 173 && b == 245:
		return true
	case a == 188 && b == 114:
		return true
	case a == 190 && b == 93:
		return true
	case a == 197 && b == 234:
		return true
	case a == 198 && b == 41:
		return true
	}
	return false
}
