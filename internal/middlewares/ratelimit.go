package middlewares

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tecisage/resell-apis/internal/config"
	"github.com/tecisage/resell-apis/internal/httpx"
	"golang.org/x/time/rate"
)

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// if we want 100 reqs per minute, then filling rate 100/60 tokens per second
type bucketLimiter struct {
	items map[string]*bucket
	limit rate.Limit // token filling speed per second
	burst int        // max tokens to store
}

func (bl *bucketLimiter) allow(ip string) bool {
	b, exists := bl.items[ip]
	if !exists {
		b = &bucket{
			limiter:  rate.NewLimiter(bl.limit, bl.burst),
			lastSeen: time.Now(),
		}
		bl.items[ip] = b
	}
	b.lastSeen = time.Now()
	return b.limiter.Allow()
}

func RateLimitMiddleware(logger *slog.Logger, limit rate.Limit, burst int, cfg config.Config) func(http.Handler) http.Handler {

	bl := &bucketLimiter{
		items: make(map[string]*bucket),
		limit: limit,
		burst: burst,
	}

	// todo : Trigger Clean up - after TTL

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// will be called on every request
			userIP := clientIP(r, cfg.TrustedProxyHeader)

			if !bl.allow(userIP) {
				logger.Info("rate limit exceeded", "user_ip", userIP, "path", r.URL.Path, "request_id", GetRequestID(r.Context()))

				retryAfter := "1"
				if limit > 0 && limit < 1 {
					retryAfter = strconv.Itoa(int(math.Ceil(1 / float64(limit))))
				}

				w.Header().Set("Retry-After", retryAfter)
				httpx.Error(w, http.StatusTooManyRequests, "Too many requests", httpx.CodeTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// client --> Load Balancer & reverse proxy ---> server (us)
// in production we will be behind a load balancer and the requests comming to us will be from the load balancer
// we can use headers like X-Real-IP and X-Forwarded-For
// proxy_set_header X-Real-IP $remote_addr;
// we generally expose two ports from reverse proxy : one for http(80) and one for https(443)

// X-Real-IP: header from nginx
func clientIP(r *http.Request, trustedProxyHeader string) string {
	if trustedProxyHeader != "" {
		return strings.TrimSpace(r.Header.Get(trustedProxyHeader))
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr) //ip:port
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
