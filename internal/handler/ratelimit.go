package handler

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
)

// ipv6PrefixBits groups IPv6 clients by network. One subscriber usually gets a
// whole /64, so limiting single IPv6 addresses would give each client 2^64
// fresh buckets.
const ipv6PrefixBits = 64

// rateLimit fails open: if the limiter's backing store is unreachable the
// request is allowed, trading abuse protection for availability.
func rateLimit(logger *slog.Logger, limiter RateLimiter, clientIPHeader string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retryAfter, err := limiter.Allow(r.Context(), clientKey(r, clientIPHeader))
		if err != nil {
			logger.WarnContext(r.Context(), "rate limiter unavailable, allowing request", slog.Any("error", err))
			allowed = true
		}
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey identifies the client to rate limit: its IPv4 address, or its
// IPv6 /64 network.
//
// It prefers the header set by a trusted reverse proxy, because behind a proxy
// every connection comes from the proxy's own address. The header must hold a
// single IP and only be configured when the proxy overwrites it, or clients
// could spoof it to dodge the limit.
func clientKey(r *http.Request, header string) string {
	raw := ""
	if header != "" {
		raw = r.Header.Get(header)
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		raw = host
	}

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, ipv6PrefixBits).Masked().String()
	}
	return addr.String()
}
