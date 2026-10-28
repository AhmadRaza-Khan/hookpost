package hookpost

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is the error (wrapped) of attempts whose destination
// resolves to a private, loopback or otherwise internal address.
var ErrBlockedAddress = errors.New("hookpost: destination address is not allowed")

// Ranges not covered by the netip.Addr predicates.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 can map to private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
}

func blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// newHTTPClient returns the default delivery client.
//
// Unless allowPrivate is set, it checks the resolved IP at dial time (after
// DNS), so a hostname that resolves or rebinds to an internal address is
// refused. Checking the URL's hostname alone would not be enough.
//
// It never follows redirects: a redirect to an internal IP is a classic SSRF
// bypass, and receivers should register their final URL anyway. A 3xx
// counts as a failed attempt.
func newHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || blocked(ip) {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route webhooks through env proxies
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
