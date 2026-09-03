// Command mockgateway stands in for the third-party services this platform
// integrates with — Razorpay, WhatsApp Business, MSG91 and OneSignal — so
// the payment, notification and dunning paths can be developed and tested
// before any of those accounts exist.
//
// WHY THIS RATHER THAN STUBBING THE CLIENTS
//
// It speaks the providers' own HTTP shapes and the real clients are pointed
// at it (MOCK_GATEWAY_URL), so their request construction, authentication
// headers, response parsing and error handling all still run. Replacing the
// clients with no-op implementations would exercise none of that, and the
// bugs that actually bite in this layer — a malformed body, a misread
// response field, an error path that swallows a failure — live precisely
// there. The whole point is that only the destination changes.
//
// It is not a Razorpay emulator. It implements the endpoints this codebase
// calls, in the shapes this codebase parses, and nothing else.
//
// Usage:
//
//	go run ./cmd/mockgateway                 # listens on :9999
//	go run ./cmd/mockgateway -addr :8081
//
// Then, in .env or app.env:
//
//	MOCK_GATEWAY_URL=http://127.0.0.1:9999
//
// config.Load refuses that variable when ENVIRONMENT=production.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// delivery is one recorded outbound message, so a developer can assert what
// the system tried to send without a real phone or inbox.
type delivery struct {
	At       time.Time         `json:"at"`
	Provider string            `json:"provider"`
	To       string            `json:"to"`
	Summary  string            `json:"summary"`
	Detail   map[string]string `json:"detail,omitempty"`
}

type recorder struct {
	mu   sync.Mutex
	list []delivery
}

func (r *recorder) add(d delivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d.At = time.Now()
	r.list = append(r.list, d)
	log.Printf("%-9s → %-18s %s", d.Provider, d.To, d.Summary)
}

func (r *recorder) all() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]delivery, len(r.list))
	copy(out, r.list)
	return out
}

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	failRate := flag.Bool("fail", false, "reject every request, to exercise failure and retry paths")
	flag.Parse()

	rec := &recorder{}
	mux := http.NewServeMux()

	// ── Razorpay: create a payment link ───────────────────────────────────
	//
	// internal/billing.RazorpayClient POSTs here and reads {id, short_url}
	// out of the response. short_url is what the portal shows the
	// subscriber, so it points back at this gateway's own /pay page — which
	// makes the renewal flow clickable end to end rather than stopping at a
	// dead link.
	mux.HandleFunc("POST /v1/payment_links", func(w http.ResponseWriter, r *http.Request) {
		if *failRate {
			http.Error(w, `{"error":{"description":"simulated failure"}}`, http.StatusBadGateway)
			return
		}
		var body struct {
			Amount      int64             `json:"amount"`
			Currency    string            `json:"currency"`
			Description string            `json:"description"`
			Notes       map[string]string `json:"notes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck

		id := fmt.Sprintf("plink_mock_%d", time.Now().UnixNano())
		rec.add(delivery{
			Provider: "razorpay",
			To:       body.Notes["subscriber_id"],
			Summary:  fmt.Sprintf("payment link %s for %.2f %s", id, float64(body.Amount)/100, body.Currency),
			Detail:   body.Notes,
		})

		writeJSON(w, map[string]any{
			"id":          id,
			"short_url":   fmt.Sprintf("http://%s/pay/%s", r.Host, id),
			"amount":      body.Amount,
			"currency":    body.Currency,
			"description": body.Description,
			"status":      "created",
		})
	})

	// A human-clickable stand-in for Razorpay's hosted checkout, so the
	// portal's renewal button leads somewhere during a demo.
	//
	// It deliberately does NOT credit the wallet. Payment reaches this
	// system through the webhook, and having this page shortcut that would
	// hide exactly the integration being tested — including the HMAC
	// verification that is the security-relevant part. The page explains how
	// to fire the webhook instead.
	mux.HandleFunc("GET /pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8">
<title>Mock checkout</title>
<style>body{font:16px/1.6 system-ui;margin:40px auto;max-width:44rem;color:#1a1a1a}
code{background:#f2f2f2;padding:2px 5px;border-radius:4px}</style>
<h1>Mock payment page</h1>
<p>Payment link <code>%s</code>. This is <strong>cmd/mockgateway</strong>, not Razorpay.</p>
<p>Clicking here does not credit the wallet, on purpose: real payments arrive
through the signed webhook, and shortcutting that would skip the HMAC
verification this page exists to let you test. Fire the webhook instead:</p>
<pre><code>go run ./scripts/mockpay -subscriber 14 -amount 599.00</code></pre>`, id)
	})

	// ── WhatsApp Business (Meta Graph) ────────────────────────────────────
	// notifications.WhatsAppClient POSTs to {baseURL}/{phoneNumberID}/messages.
	mux.HandleFunc("POST /{phoneNumberID}/messages", func(w http.ResponseWriter, r *http.Request) {
		if *failRate {
			http.Error(w, `{"error":{"message":"simulated failure"}}`, http.StatusBadGateway)
			return
		}
		var body struct {
			To       string `json:"to"`
			Type     string `json:"type"`
			Template struct {
				Name string `json:"name"`
			} `json:"template"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck

		rec.add(delivery{
			Provider: "whatsapp",
			To:       body.To,
			Summary:  fmt.Sprintf("template %q", body.Template.Name),
		})
		writeJSON(w, map[string]any{
			"messaging_product": "whatsapp",
			"messages":          []map[string]string{{"id": fmt.Sprintf("wamid.mock%d", time.Now().UnixNano())}},
		})
	})

	// ── MSG91 (SMS) ───────────────────────────────────────────────────────
	// A GET with query parameters, and a plain-text body in response.
	mux.HandleFunc("GET /api/sendhttp.php", func(w http.ResponseWriter, r *http.Request) {
		if *failRate {
			http.Error(w, "simulated failure", http.StatusBadGateway)
			return
		}
		q := r.URL.Query()
		msg := q.Get("message")
		rec.add(delivery{
			Provider: "sms",
			To:       q.Get("mobiles"),
			Summary:  truncate(msg, 70),
			Detail:   map[string]string{"sender": q.Get("sender"), "message": msg},
		})
		fmt.Fprintf(w, "mock-msgid-%d", time.Now().UnixNano())
	})

	// ── OneSignal (push) ──────────────────────────────────────────────────
	mux.HandleFunc("POST /api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		if *failRate {
			http.Error(w, `{"errors":["simulated failure"]}`, http.StatusBadGateway)
			return
		}
		var body struct {
			IncludePlayerIDs []string          `json:"include_player_ids"`
			Contents         map[string]string `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck

		rec.add(delivery{
			Provider: "push",
			To:       strings.Join(body.IncludePlayerIDs, ","),
			Summary:  truncate(body.Contents["en"], 70),
		})
		writeJSON(w, map[string]any{"id": fmt.Sprintf("mock-push-%d", time.Now().UnixNano()), "recipients": len(body.IncludePlayerIDs)})
	})

	// ── Inspection ────────────────────────────────────────────────────────
	//
	// What was sent, as JSON. This is what makes the gateway useful beyond
	// "it did not error": an integration test can assert that a dunning run
	// actually attempted an SMS to the right number with the right text.
	// Returns an object with an explicit count rather than a bare array.
	//
	// A bare array is ambiguous to count from PowerShell: Invoke-RestMethod
	// unrolls a JSON array onto the pipeline, so `(Invoke-RestMethod ...).Count`
	// and `@(Invoke-RestMethod ...).Count` disagree — 2 versus 1 for the same
	// two records. scripts/verify_money_path.ps1 used one form for its
	// baseline and the other in its poll loop, compared 1 against 2, and
	// reported that no notification had been delivered when two had. A scalar
	// the client does not have to interpret removes the trap.
	mux.HandleFunc("GET /_deliveries", func(w http.ResponseWriter, _ *http.Request) {
		all := rec.all()
		writeJSON(w, map[string]any{"count": len(all), "deliveries": all})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ISP BSS mock gateway\n\n"+
			"Stands in for Razorpay, WhatsApp, MSG91 and OneSignal.\n"+
			"Point the services at it with MOCK_GATEWAY_URL.\n\n"+
			"GET /_deliveries   everything sent so far, as JSON\n")
	})

	log.SetFlags(log.Ltime)
	log.Printf("mock gateway listening on %s", *addr)
	if *failRate {
		log.Printf("-fail is set: every request will be rejected, to exercise retry and dead-letter paths")
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "mockgateway: %v\n", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
