package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestStatusClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code int
		want string
	}{
		{100, "1xx"},
		{200, "2xx"},
		{201, "2xx"},
		{301, "3xx"},
		{404, "4xx"},
		{500, "5xx"},
		{599, "5xx"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := StatusClass(tc.code); got != tc.want {
				t.Errorf("StatusClass(%d) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestMiddlewareRecordsRequest(t *testing.T) {
	handler := Middleware("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	// Scrape the registry and look for the recorded series.
	promSrv := httptest.NewServer(promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	defer promSrv.Close()

	resp, err := http.Get(promSrv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	wantMetric := `paladin_http_requests_total{method="GET",route="/healthz",status="4xx"}`
	if !strings.Contains(body, wantMetric) {
		t.Errorf("scrape missing expected series %q\nbody=%s", wantMetric, body)
	}
}
