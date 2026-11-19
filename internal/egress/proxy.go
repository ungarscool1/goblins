// Package egress is an allow-listing HTTPS proxy for workloads that need the internet, such as
// agents. Their network reaches nothing but this proxy, which only opens tunnels to the hosts
// an operator listed, so a prompt-injected agent cannot send what it reads anywhere else.
package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ungarscool1/goblins/internal/observability"
)

// Options configures a Proxy.
type Options struct {
	// Allow lists host names, exactly or as *.example.com for any subdomain. Nothing else is
	// reachable.
	Allow []string
	// Ports are the destination ports allowed, 443 by default.
	Ports []int
	// AllowPrivate lets tunnels reach private, loopback and link-local addresses. Without it,
	// a listed name that resolves to one is refused, so DNS cannot point a tunnel inside.
	AllowPrivate bool
	// MaxConns bounds concurrent tunnels.
	MaxConns int
	// Lookup resolves a host name; by default, the system resolver.
	Lookup  func(ctx context.Context, host string) ([]netip.Addr, error)
	Dialer  *net.Dialer
	Log     *slog.Logger
	Metrics *observability.Metrics
}

// Proxy implements http.Handler for CONNECT requests only.
type Proxy struct {
	o     Options
	slots chan struct{}
}

// New returns a proxy. An empty allow-list is accepted and allows nothing.
func New(o Options) (*Proxy, error) {
	for _, h := range o.Allow {
		if !validPattern(h) {
			return nil, errors.New("not a host name or *.domain: " + strconv.Quote(h))
		}
	}
	if len(o.Ports) == 0 {
		o.Ports = []int{443}
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 256
	}
	if o.Lookup == nil {
		o.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Dialer == nil {
		o.Dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Proxy{o: o, slots: make(chan struct{}, o.MaxConns)}, nil
}

func validPattern(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || len(h) > 253 || strings.ContainsAny(h, "/:@ ") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return true
}

// Allowed reports whether a host name is on the list. Names are compared lower case, without
// a trailing dot, and an IP address is never allowed by a name.
func (p *Proxy) Allowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, pat := range p.o.Allow {
		pat = strings.ToLower(pat)
		if rest, ok := strings.CutPrefix(pat, "*."); ok {
			if strings.HasSuffix(host, "."+rest) {
				return true
			}
		} else if host == pat {
			return true
		}
	}
	return false
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// Plain HTTP would expose what is sent, and could be pointed anywhere by a Host header.
		p.deny(w, r.Host, "only HTTPS tunnels are proxied", http.StatusMethodNotAllowed)
		return
	}
	host, portText, err := net.SplitHostPort(r.Host)
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	port, perr := strconv.Atoi(portText)
	if err != nil || perr != nil || !slices.Contains(p.o.Ports, port) || !p.Allowed(host) {
		p.deny(w, r.Host, "destination not allowed", http.StatusForbidden)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		p.deny(w, r.Host, "too many tunnels", http.StatusServiceUnavailable)
		return
	}
	upstream, err := p.dial(r.Context(), host, port)
	if err != nil {
		p.deny(w, r.Host, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		p.deny(w, r.Host, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	p.o.Metrics.Inc("goblins_egress_tunnels_total", observability.Labels{"outcome", "allowed"})
	p.o.Log.Info("egress allowed", "host", host, "port", port)
	splice(client, buf, upstream)
}

// dial resolves the host and connects to an address it checked, never to the name again, so
// a second DNS answer cannot change where the tunnel goes.
func (p *Proxy) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := p.o.Lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, errors.New("cannot resolve the destination: " + errText(err))
	}
	for _, a := range addrs {
		if !p.o.AllowPrivate && !public(a) {
			return nil, errors.New("the destination resolves to a private address")
		}
	}
	var last error
	for _, a := range addrs {
		c, err := p.o.Dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(),
			uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, errors.New("cannot connect to the destination: " + errText(last))
}

func errText(err error) string {
	if err == nil {
		return "no address"
	}
	return err.Error()
}

// public reports whether an address is on the public internet: not loopback, private,
// link-local (which holds cloud metadata services), shared, multicast or unspecified.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, also used inside clusters
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can map back to private IPv4
}

func (p *Proxy) deny(w http.ResponseWriter, target, why string, code int) {
	p.o.Metrics.Inc("goblins_egress_tunnels_total", observability.Labels{"outcome", "denied"})
	p.o.Log.Warn("egress denied", "target", target, "reason", why)
	http.Error(w, why, code)
}

func splice(client net.Conn, buf io.Reader, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, buf)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}()
	wg.Wait()
}
