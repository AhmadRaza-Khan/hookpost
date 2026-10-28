package hookpost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "10.0.0.5", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0", "0.1.2.3", "::",
		"100.64.0.1", "198.18.0.1", "255.255.255.255", "224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2001:db8::1",
	} {
		if !blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s: want blocked", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700::1111", "100.128.0.1"} {
		if blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s: want allowed", s)
		}
	}
}

func TestHTTPClientGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	get := func(hc *http.Client) error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(newHTTPClient(time.Second, false)); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("guarded client: got %v, want ErrBlockedAddress", err)
	}
	if err := get(newHTTPClient(time.Second, true)); err != nil {
		t.Fatalf("private allowed: %v", err)
	}
}

func TestHTTPClientIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	tr := newHTTPClient(time.Second, false).Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("transport uses a proxy")
	}
}
