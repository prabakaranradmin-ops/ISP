package walledgarden

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const target = "https://portal.example.net/ui/login?suspended=1"

func get(t *testing.T, h http.Handler, method, rawURL string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, rawURL, nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Whatever page the subscriber tried to open, including the connectivity
// checks phones and laptops use to decide whether to show their sign-in
// prompt, they are sent to the one configured address.
func TestFR_BIL_004_EveryRedirectedRequestLandsOnThePaymentPage(t *testing.T) {
	h, err := NewHandler(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{
		"http://example.com/",
		"http://news.example.org/2026/11/a-story?id=7",
		"http://connectivitycheck.gstatic.com/generate_204",
		"http://captive.apple.com/hotspot-detect.html",
		"http://www.msftconnecttest.com/connecttest.txt",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead} {
			rec := get(t, h, method, u, nil)
			if rec.Code != http.StatusFound {
				t.Errorf("%s %s: %d, want 302", method, u, rec.Code)
			}
			if got := rec.Header().Get("Location"); got != target {
				t.Errorf("%s %s: Location %q", method, u, got)
			}
			if method == http.MethodHead && rec.Body.Len() != 0 {
				t.Errorf("HEAD %s carried a body", u)
			}
		}
	}
}

// A permanent or cacheable redirect would be kept by the browser for the host
// it was issued under, and would go on sending the subscriber to the payment
// page after they had paid.
func TestFR_BIL_004_TheRedirectIsNeverRemembered(t *testing.T) {
	h, _ := NewHandler(target)
	rec := get(t, h, http.MethodGet, "http://example.com/", nil)
	if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusPermanentRedirect {
		t.Fatalf("a permanent redirect (%d)", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q lets the redirect be cached", cc)
	}
}

// Nothing in the request reaches the response: not the Host it named, not
// its path, not a header. The target is fixed, so the redirector cannot be
// turned into an open redirect or made to echo markup.
func TestFR_BIL_004_NothingFromTheRequestIsEchoed(t *testing.T) {
	h, _ := NewHandler(target)
	rec := get(t, h, http.MethodGet, `http://evil.example/"><script>alert(1)</script>`, func(r *http.Request) {
		r.Host = "attacker.example"
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		r.Header.Set("Referer", "http://attacker.example/")
	})
	if rec.Header().Get("Location") != target {
		t.Errorf("Location %q", rec.Header().Get("Location"))
	}
	body, _ := io.ReadAll(rec.Body)
	for _, bad := range []string{"attacker", "evil", "<script>"} {
		if strings.Contains(string(body), bad) || strings.Contains(rec.Header().Get("Location"), bad) {
			t.Errorf("the response carries %q from the request", bad)
		}
	}
	// A browser that does not follow redirects still gets a link it can use.
	if !strings.Contains(string(body), `href="https://portal.example.net/ui/login?suspended=1"`) {
		t.Errorf("body %s", body)
	}
}

// A target that is not an absolute web address is refused at startup, not
// served to suspended subscribers as a broken redirect.
func TestFR_BIL_004_ABadTargetIsRefusedAtStartup(t *testing.T) {
	for _, bad := range []string{"", "portal.example.net", "/ui/login", "ftp://portal.example.net/", "https://", "http://[::1"} {
		if _, err := NewHandler(bad); err == nil {
			t.Errorf("%q was accepted as a redirect target", bad)
		}
		if _, err := NewServer(":0", bad); err == nil {
			t.Errorf("%q was accepted by NewServer", bad)
		}
	}
	// And the markup in a target is escaped in the fallback link.
	h, err := NewHandler(`https://portal.example.net/ui/login?a="b"`)
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, h, http.MethodGet, "http://example.com/", nil).Body.String()
	if strings.Contains(body, `a="b"`) {
		t.Errorf("an unescaped quote in the link: %s", body)
	}
}

// Over a real socket, the way a router's rewritten request arrives: the
// browser asked for some other host, and gets the redirect back.
func TestFR_BIL_004_TheRedirectorAnswersOverARealSocket(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", target)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck // closed below
	t.Cleanup(func() { srv.Close() })

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/generate_204", nil)
	req.Host = "connectivitycheck.gstatic.com" // what the device thought it was talking to
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != target {
		t.Errorf("%d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestTheServerIsBounded(t *testing.T) {
	s, err := NewServer(":8088", target)
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != ":8088" || s.ReadHeaderTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 || s.MaxHeaderBytes == 0 {
		t.Errorf("server %+v", s)
	}
}
