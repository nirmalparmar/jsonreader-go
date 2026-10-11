package httpx

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is a keyed token bucket (one bucket per client IP). Buckets idle
// long enough to have refilled completely are evicted by a janitor goroutine.
type RateLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(perMinute, burst int) *RateLimiter {
	if perMinute <= 0 {
		perMinute = 1
	}
	if burst <= 0 {
		burst = 1
	}
	rl := &RateLimiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
	}
	go rl.janitor()
	return rl
}

// Allow consumes one token for key. When it fails it reports how long the
// caller should wait before retrying.
func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens = math.Min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rl.rate * float64(time.Second))
	return false, wait
}

func (rl *RateLimiter) janitor() {
	full := time.Duration(rl.burst/rl.rate*float64(time.Second)) + time.Minute
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for now := range t.C {
		rl.mu.Lock()
		for k, b := range rl.buckets {
			if now.Sub(b.last) > full {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}

// Limit wraps a handler with per-client-IP rate limiting.
func (rl *RateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if ok, wait := rl.Allow(ClientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			Error(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}
