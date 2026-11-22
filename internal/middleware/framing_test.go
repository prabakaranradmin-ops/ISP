package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Every response carries the framing policy — including errors and 404s,
// which can be framed as readily as any other page.
func TestEveryResponseRefusesCrossOriginFraming(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /staff/login", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	})
	h := middleware.DenyCrossOriginFraming(mux)

	for _, path := range []string{"/staff/login", "/boom", "/no-such-page"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'self'" {
			t.Errorf("%s (%d): CSP %q", path, rec.Code, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Errorf("%s (%d): X-Frame-Options %q", path, rec.Code, got)
		}
	}
}
