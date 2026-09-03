// Command mockpay delivers a correctly-signed Razorpay `payment.captured`
// webhook to a locally running API, so the whole money path can be exercised
// without a Razorpay account.
//
// WHY THIS WORKS WITHOUT ANY CREDENTIALS
//
// Razorpay authenticates its webhooks by signing the body with the shared
// secret in RAZORPAY_WEBHOOK_SECRET — a value this deployment chooses, not
// one Razorpay issues. So a payload signed with that same secret is
// indistinguishable from a real one to the receiving code, and the entire
// downstream chain runs for real:
//
//	signature verification → wallet credit (double-entry) → franchise
//	commission settlement → GL posting → partner webhook emission
//
// The only thing simulated is the fact that money moved. Everything the
// system does in response is genuine, which is what makes this worth having
// rather than calling the internal recharge API directly.
//
// Usage:
//
//	go run ./scripts/mockpay -subscriber 14 -amount 599.00
//	go run ./scripts/mockpay -subscriber 14 -amount 599.00 -bad-signature
//
// The secret is read from RAZORPAY_WEBHOOK_SECRET, or -secret.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	var (
		apiURL       = flag.String("url", "https://localhost/webhooks/razorpay", "webhook endpoint")
		secret       = flag.String("secret", os.Getenv("RAZORPAY_WEBHOOK_SECRET"), "RAZORPAY_WEBHOOK_SECRET")
		subscriberID = flag.Int("subscriber", 0, "subscriber id to credit (required)")
		amount       = flag.String("amount", "599.00", "amount in rupees")
		paymentID    = flag.String("payment-id", "", "Razorpay payment id (default: generated, and it is the idempotency key)")
		badSignature = flag.Bool("bad-signature", false, "send a deliberately wrong signature, to prove the endpoint rejects it")
		insecure     = flag.Bool("insecure", true, "skip TLS verification (the demo stack ships a self-signed cert)")
	)
	flag.Parse()

	if *subscriberID == 0 {
		fatal("-subscriber is required")
	}
	if *secret == "" {
		fatal("no webhook secret: set RAZORPAY_WEBHOOK_SECRET or pass -secret.\n" +
			"      It is in app.env (native install) or .env (compose).")
	}

	rupees, err := strconv.ParseFloat(*amount, 64)
	if err != nil {
		fatal(fmt.Sprintf("-amount %q is not a number", *amount))
	}
	// Razorpay reports paise, and the handler divides by 100. Sending rupees
	// here would credit a hundredth of the intended amount — the kind of
	// mistake that looks like a rounding bug for an afternoon.
	paise := int64(rupees * 100)

	id := *paymentID
	if id == "" {
		id = fmt.Sprintf("pay_mock%d", time.Now().UnixNano())
	}

	// The subset of Razorpay's payload the handler actually reads. Notes
	// carry subscriber_id, which is how the payment is matched to an
	// account — order creation sets it in production.
	payload := map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{
			"payment": map[string]any{
				"entity": map[string]any{
					"id":       id,
					"amount":   paise,
					"currency": "INR",
					"status":   "captured",
					"notes": map[string]string{
						"subscriber_id": strconv.Itoa(*subscriberID),
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fatal(err.Error())
	}

	mac := hmac.New(sha256.New, []byte(*secret))
	mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	if *badSignature {
		signature = "0000000000000000000000000000000000000000000000000000000000000000"
	}

	req, err := http.NewRequest(http.MethodPost, *apiURL, bytes.NewReader(body))
	if err != nil {
		fatal(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", signature)

	client := &http.Client{Timeout: 20 * time.Second}
	if *insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local self-signed cert
		}
	}

	fmt.Printf("POST %s\n", *apiURL)
	fmt.Printf("  subscriber : %d\n", *subscriberID)
	fmt.Printf("  amount     : ₹%s (%d paise)\n", *amount, paise)
	fmt.Printf("  payment id : %s\n", id)
	if *badSignature {
		fmt.Printf("  signature  : DELIBERATELY INVALID — expecting rejection\n")
	}

	resp, err := client.Do(req)
	if err != nil {
		fatal(fmt.Sprintf("request failed: %v", err))
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, _ := io.ReadAll(resp.Body) //nolint:errcheck

	fmt.Printf("\n%s\n", resp.Status)
	if len(bytes.TrimSpace(respBody)) > 0 {
		fmt.Printf("%s\n", respBody)
	}

	switch {
	case *badSignature && resp.StatusCode == http.StatusOK:
		fmt.Println("\nFAIL: an invalid signature was accepted. Anyone could credit any wallet.")
		os.Exit(1)
	case *badSignature:
		fmt.Println("\nCorrect: the invalid signature was rejected.")
	case resp.StatusCode == http.StatusOK:
		fmt.Printf("\nAccepted. The wallet credit, commission settlement and GL posting all ran for real.\n"+
			"Re-run with -payment-id %s to check idempotency: the balance must not move twice.\n", id)
	default:
		fmt.Println("\nNot accepted — check the API log.")
		os.Exit(1)
	}
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "mockpay: %s\n", msg)
	os.Exit(1)
}
