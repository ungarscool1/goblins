package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// echo listens on loopback and repeats one line back.
func echo(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, "echo "+line)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func proxy(t *testing.T, o Options) string {
	t.Helper()
	if o.Lookup == nil {
		o.Lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "allowed.test", "api.allowed.test":
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			case "rebind.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34"),
					netip.MustParseAddr("169.254.169.254")}, nil
			}
			return nil, fmt.Errorf("no such host")
		}
	}
	o.Log = slog.New(slog.DiscardHandler)
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// connect opens a tunnel and returns the proxy's status line and, if open, what came back.
func connect(t *testing.T, proxyAddr, target string) (string, string) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.Status, ""
	}
	_, _ = io.WriteString(c, "hello\n")
	line, _ := r.ReadString('\n')
	return resp.Status, line
}

func TestOnlyListedHostsAreReachable(t *testing.T) {
	port := echo(t)
	addr := proxy(t, Options{Allow: []string{"allowed.test", "*.allowed.test"}, Ports: []int{port},
		AllowPrivate: true})
	p := strconv.Itoa(port)
	for target, want := range map[string]string{
		"allowed.test:" + p:      "200",
		"API.Allowed.Test.:" + p: "200",
		"other.test:" + p:        "403",
		"allowed.test.evil:" + p: "403",
		"127.0.0.1:" + p:         "403",
		"allowed.test:22":        "403",
	} {
		status, body := connect(t, addr, target)
		if !strings.HasPrefix(status, want) {
			t.Errorf("%s: %s", target, status)
		}
		if want == "200" && body != "echo hello\n" {
			t.Errorf("%s: tunnel carried %q", target, body)
		}
	}
}

// A listed name that resolves inside the network is refused, so DNS cannot redirect a tunnel
// to the cluster, the host or a metadata service.
func TestPrivateAddressesAreRefused(t *testing.T) {
	port := echo(t)
	addr := proxy(t, Options{Allow: []string{"allowed.test", "rebind.test"}, Ports: []int{port}})
	for _, host := range []string{"allowed.test", "rebind.test"} {
		if status, _ := connect(t, addr, host+":"+strconv.Itoa(port)); !strings.HasPrefix(status,
			"502") {
			t.Errorf("%s: %s", host, status)
		}
	}
}

func TestPlainHTTPIsRefused(t *testing.T) {
	addr := proxy(t, Options{Allow: []string{"allowed.test"}})
	req, _ := http.NewRequest(http.MethodGet, "http://allowed.test/", nil)
	c := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*urlT, error) {
		return parse("http://" + addr)
	}}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain HTTP: %d", resp.StatusCode)
	}
}

// The proxy answers its own health probe, which is not a request through it.
func TestHealthProbe(t *testing.T) {
	addr := proxy(t, Options{Allow: []string{"allowed.test"}})
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe: %d", resp.StatusCode)
	}
}

func TestPatternsAndAddresses(t *testing.T) {
	if _, err := New(Options{Allow: []string{"https://x.test"}}); err == nil {
		t.Error("a URL was accepted as a host")
	}
	for _, a := range []string{"10.1.2.3", "172.16.0.1", "192.168.1.1", "127.0.0.1",
		"169.254.169.254",
		"100.64.0.1", "::1", "fe80::1", "fd00::1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "0.0.0.0"} {
		if public(netip.MustParseAddr(a)) {
			t.Errorf("%s counted as public", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !public(netip.MustParseAddr(a)) {
			t.Errorf("%s counted as private", a)
		}
	}
}

type urlT = url.URL

func parse(s string) (*url.URL, error) { return url.Parse(s) }
