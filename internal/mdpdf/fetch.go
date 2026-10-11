package mdpdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errBlockedAddr = errors.New("destination address is not publicly routable")

// nonPublic lists ranges that IsPrivate/IsLoopback/... do not cover but that
// must never be reachable from user-supplied URLs.
var nonPublic = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
	"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// safeFetcher downloads documents' remote assets (images, fonts, CSS). The
// address check runs in the dialer's Control hook, i.e. on the exact IP being
// connected to, so DNS rebinding and redirects to internal hosts are caught.
type safeFetcher struct {
	client      *http.Client
	maxResource int64
}

func newSafeFetcher(maxResource int64) *safeFetcher {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !isPublic(ip) {
				return errBlockedAddr
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                  nil, // never route through env-configured proxies
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  8 * time.Second,
		MaxIdleConns:           64,
		IdleConnTimeout:        60 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &safeFetcher{
		maxResource: maxResource,
		client: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 4 {
					return errors.New("too many redirects")
				}
				return checkURL(req.URL)
			},
		},
	}
}

func checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	if u.User != nil {
		return errors.New("credentials in URL not allowed")
	}
	switch u.Port() {
	case "", "80", "443":
	default:
		return fmt.Errorf("port %s not allowed", u.Port())
	}
	return nil
}

type fetched struct {
	status      int
	contentType string
	body        []byte
}

func (f *safeFetcher) get(ctx context.Context, raw string, budget *fetchBudget) (*fetched, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MarkdownPDF/1.0; +https://www.jsonreader.com)")
	req.Header.Set("Accept", "image/avif,image/webp,image/svg+xml,image/*,text/css,font/*,*/*;q=0.5")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := min(f.maxResource, budget.remaining())
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("resource exceeds size budget")
	}
	budget.spend(int64(len(body)))
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	// Never let a remote response masquerade as an HTML document.
	if strings.HasPrefix(strings.ToLower(ct), "text/html") {
		ct = "text/plain"
	}
	return &fetched{status: resp.StatusCode, contentType: ct, body: body}, nil
}

// fetchBudget caps how much a single document may download.
type fetchBudget struct {
	mu       sync.Mutex
	requests int
	bytes    int64
	maxReq   int
	maxBytes int64
}

func (b *fetchBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requests >= b.maxReq {
		return false
	}
	b.requests++
	return true
}

func (b *fetchBudget) remaining() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.maxBytes-b.bytes, 0)
}

func (b *fetchBudget) spend(n int64) {
	b.mu.Lock()
	b.bytes += n
	b.mu.Unlock()
}
