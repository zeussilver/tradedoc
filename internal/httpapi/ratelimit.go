package httpapi

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Rate limits, per client IP, kept in this process's memory.
//
// What these buckets can and can't do: they only see the requests that reach
// this one server instance. On Lambda each instance handles one request at a
// time, so concurrent requests start more instances, and each new instance
// starts with an empty map (full buckets). Here they are a cheap first filter
// against a client hammering one warm instance, not a hard limit. The hard
// limits on paid model calls are the daily counters in the store (see
// takeModelQuota in model.go), which every instance shares.
//
// General API: opening a shipment in the UI fires about ten requests at once,
// and after that a person clicks a few times a minute. A burst of 30 and then
// 5 per second never gets in a real user's way, but slows a script flooding
// the API.
//
// Model routes: each call costs money and takes 30-90 s. One shipment needs
// three (read the invoice, read the packing list, suggest codes), so a burst
// of 4 allows that plus one retry, then one more every 30 s per instance.
const (
	apiRate    rate.Limit = 5
	apiBurst              = 30
	modelRate             = rate.Limit(1.0 / 30) // one every 30 s
	modelBurst            = 4
)

// Idle addresses are forgotten so the map can't grow forever. idleTTL must be
// longer than the time a bucket takes to refill completely (4 x 30 s = 2 min
// for the model bucket): a forgotten address restarts with a full bucket,
// which is what it would have had anyway, so forgetting never loosens a limit.
const (
	idleTTL    = 10 * time.Minute
	sweepEvery = time.Minute
)

// rateLimiter keeps one token bucket per client. Each request takes a token;
// tokens refill at a steady rate up to the burst size.
type rateLimiter struct {
	mu        sync.Mutex
	rate      rate.Limit
	burst     int
	clients   map[string]*client
	lastSweep time.Time
}

type client struct {
	bucket   *rate.Limiter
	lastSeen time.Time
}

func newRateLimiter(r rate.Limit, burst int) *rateLimiter {
	return &rateLimiter{rate: r, burst: burst, clients: map[string]*client{}}
}

// allow takes one token for key. If there is none it reports false and how
// long the client should wait: a token refills every 1/rate, so retrying
// after that is always enough.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	c, ok := l.clients[key]
	if !ok {
		c = &client{bucket: rate.NewLimiter(l.rate, l.burst)}
		l.clients[key] = c
	}
	c.lastSeen = now
	if c.bucket.AllowN(now, 1) {
		return true, 0
	}
	return false, time.Duration(float64(time.Second) / float64(l.rate))
}

// sweep deletes clients idle for longer than idleTTL. It runs at most once a
// minute, inside a request, so there is no background goroutine to manage
// (which also suits Lambda, where nothing runs between requests).
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for key, c := range l.clients {
		if now.Sub(c.lastSeen) > idleTTL {
			delete(l.clients, key)
		}
	}
}

// size is the number of clients being tracked (used by tests).
func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.clients)
}

// limit answers 429 with Retry-After when the client is out of tokens.
func limit(l *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(clientIP(r.RemoteAddr), time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; please slow down", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAPIRateLimit limits every /v1/ request. Static files and /healthz are
// cheap to serve, so they are left alone.
func (s *Server) withAPIRateLimit(next http.Handler) http.Handler {
	limited := limit(s.apiLimit, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			limited.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withModelLimit adds the much stricter in-memory bucket in front of a model
// route. The handler then also takes the shared daily quotas (takeModelQuota).
func (s *Server) withModelLimit(next http.Handler) http.Handler {
	return limit(s.modelLimit, next)
}

// clientIP is the address the connection came from. X-Forwarded-For is
// ignored on purpose: any client can set that header, so trusting it would
// let an attacker claim a fresh "IP" on every request and never be limited.
func clientIP(remoteAddr string) string {
	host := remoteAddr
	// A normal server gives "ip:port"; a Lambda Function URL gives a bare IP.
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host // unexpected shape: still limit by the raw string
	}
	// One IPv6 user is normally given a whole /64 block. Limiting single
	// addresses would let them rotate through billions of them, so the
	// whole block shares one bucket (and one daily model quota).
	if addr.Is6() && !addr.Is4In6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.Unmap().String()
}
