// Package ratelimit provides an in-memory per-key token bucket limiter and proxy-aware client IP resolution.
package ratelimit

import (
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTrustedProxies covers loopback, RFC1918 and IPv6 ULA ranges, so a reverse proxy (Caddy)
// on the same host or in a Docker network is trusted out of the box.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// DefaultMaxKeys bounds the bucket map so a flood of distinct keys cannot exhaust memory.
const DefaultMaxKeys = 100_000

// IPResolver decides which address identifies the client.
type IPResolver struct {
	trusted []netip.Prefix
}

// NewIPResolver parses a comma-separated CIDR list; an empty string means DefaultTrustedProxies.
// Bare IPs are accepted as /32 or /128.
func NewIPResolver(cidrs string) (*IPResolver, error) {
	if strings.TrimSpace(cidrs) == "" {
		cidrs = DefaultTrustedProxies
	}
	var prefixes []netip.Prefix
	for _, part := range strings.Split(cidrs, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, err
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, p.Masked())
	}
	return &IPResolver{trusted: prefixes}, nil
}

var defaultResolver, _ = NewIPResolver("")

func (res *IPResolver) isTrusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range res.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP returns the RemoteAddr host unless the direct peer is a trusted proxy. In that case it
// walks X-Forwarded-For right to left and returns the first hop that is not itself trusted (the
// address the outermost trusted proxy saw), falling back to the leftmost entry.
func (res *IPResolver) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !res.isTrusted(peer) {
		return host
	}

	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	if len(hops) == 0 {
		return host
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			// Garbage in a position we would otherwise trust: stop here and use the peer.
			return host
		}
		if !res.isTrusted(addr) {
			return addr.Unmap().String()
		}
	}
	if addr, err := netip.ParseAddr(hops[0]); err == nil {
		return addr.Unmap().String()
	}
	return host
}

// ClientIP resolves with the default trusted proxy list.
func ClientIP(r *http.Request) string { return defaultResolver.ClientIP(r) }

// Limiter is a per-key token bucket held in memory; fine for a single API instance.
type Limiter struct {
	mu        sync.Mutex
	capacity  float64
	perSecond float64
	maxKeys   int
	buckets   map[string]*bucket
	lastSweep time.Time
	fullLog   sync.Once
	resolver  *IPResolver
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing perMinute requests per key (bursting up to perMinute).
// perMinute <= 0 disables limiting. A nil resolver uses the default trusted proxies.
func New(perMinute int, resolver *IPResolver) *Limiter {
	if resolver == nil {
		resolver = defaultResolver
	}
	return &Limiter{
		capacity:  float64(perMinute),
		perSecond: float64(perMinute) / 60,
		maxKeys:   DefaultMaxKeys,
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
		resolver:  resolver,
		now:       time.Now,
	}
}

// SetMaxKeys overrides the bucket map cap (mainly for tests).
func (l *Limiter) SetMaxKeys(n int) { l.maxKeys = n }

// Allow consumes one token for key.
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.Check(key)
	return ok
}

// Check consumes one token for key; when denied it also returns how long until a token is available.
func (l *Limiter) Check(key string) (bool, time.Duration) {
	if l == nil || l.capacity <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) > 10*time.Minute {
		l.sweepLocked(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
		}
		if len(l.buckets) >= l.maxKeys {
			l.fullLog.Do(func() {
				slog.Warn("rate limiter key table full; rejecting new clients", "max_keys", l.maxKeys)
			})
			return false, time.Minute
		}
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.capacity, b.tokens+now.Sub(b.last).Seconds()*l.perSecond)
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweepLocked drops buckets that have refilled completely (idle long enough to be indistinguishable from new).
func (l *Limiter) sweepLocked(now time.Time) {
	full := time.Duration(l.capacity/l.perSecond*float64(time.Second)) + time.Second
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// Middleware rejects requests over the per-client-IP limit with a JSON envelope 429 and Retry-After.
func Middleware(l *Limiter, next http.Handler) http.Handler {
	if l == nil || l.capacity <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.Check(l.resolver.ClientIP(r))
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		WriteTooManyRequests(w, r, wait)
	})
}

// RetryAfterSeconds formats a wait as whole seconds (minimum 1) for the Retry-After header.
func RetryAfterSeconds(wait time.Duration) string {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// WriteTooManyRequests renders the standard API envelope without depending on the httpapi package.
// The request id is taken from the response header set by the request-id middleware.
func WriteTooManyRequests(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	reqID := w.Header().Get("X-Request-Id")
	if reqID == "" {
		reqID = r.Header.Get("X-Request-Id")
	}
	w.Header().Set("Retry-After", RetryAfterSeconds(wait))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":       "too many requests",
		"code":        "rate_limited",
		"request_id":  reqID,
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
}
