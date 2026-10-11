package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestRealIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("172.16.0.0/12")}
	var got string
	h := RealIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIP(r) }))

	cases := []struct {
		name, remote, xri, xff, want string
	}{
		{"direct client ignores spoofed headers", "203.0.113.9:5000", "1.2.3.4", "5.6.7.8", "203.0.113.9"},
		{"trusted proxy X-Real-IP", "172.18.0.1:5000", "198.51.100.7", "", "198.51.100.7"},
		{"trusted proxy XFF rightmost untrusted", "127.0.0.1:5000", "", "9.9.9.9, 198.51.100.7, 172.18.0.5", "198.51.100.7"},
		{"trusted proxy garbage header", "127.0.0.1:5000", "not-an-ip", "", "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xri != "" {
			r.Header.Set("X-Real-IP", c.xri)
		}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCORSRejectsForeignOrigins(t *testing.T) {
	o := NewOrigins([]string{"https://www.jsonreader.com"}, false)
	h := o.CORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	r := httptest.NewRequest(http.MethodPost, "/v1/run/python", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("foreign origin: status %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodOptions, "/v1/run/python", nil)
	r.Header.Set("Origin", "https://www.jsonreader.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://www.jsonreader.com" {
		t.Errorf("preflight: status %d, ACAO %q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(60, 3)
	for i := range 3 {
		if ok, _ := rl.Allow("a"); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	if ok, wait := rl.Allow("a"); ok || wait <= 0 {
		t.Fatal("4th request should be limited")
	}
	if ok, _ := rl.Allow("b"); !ok {
		t.Fatal("other clients are independent")
	}
}
