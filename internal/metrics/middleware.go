package metrics

import (
	"net/http"
	"time"
)

// Middleware wraps an http.Handler and records one observation per request:
//
//   - increments paladin_http_requests_total{method,route,status}
//   - observes paladin_http_request_duration_seconds{method,route}
//
// The "route" label must be a stable route pattern (e.g. /api/v1/config/*),
// not the raw URL, to avoid unbounded label cardinality. When we do not know
// the route (e.g. /metrics, /healthz), pass the path literal — it's a closed
// set.
func Middleware(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start).Seconds()

		HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(elapsed)
		HTTPRequestsTotal.WithLabelValues(r.Method, route, StatusClass(rec.status)).Inc()
	})
}

// statusRecorder captures the status code written to the underlying
// ResponseWriter. It is not safe for concurrent use by definition — one
// recorder per request, which is what the HTTP server gives us.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write implicitly writes a 200 status if WriteHeader was not called.
// The net/http default machinery does this, so our shadow counter must too.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true // status already defaults to 200 in the constructor
	}
	return r.ResponseWriter.Write(b)
}
