package notifications_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// FR-NOTIF-010 requires every WhatsApp message to go out under a template
// pre-approved with Meta. Meta addresses templates by name, not by the
// internal TMPL-00N id, so the mapping is the control: a wrong or missing
// name is silently rejected by Meta at send time rather than caught here.

func TestFR_NOTIF_010_TemplateNameFor_ResolvesRegisteredTemplates(t *testing.T) {
	// The assignments in 00_IDX_Master_Traceability_Index.md, which is the
	// authority for what each id means.
	//
	// This list was previously 004 service_suspended, 005 payment_received,
	// 006 plan_expiring, 007 promotional_offer — none of which are the index's
	// assignments from 005 on. Not deriving it from the map under test was the
	// right instinct, but it was transcribed from the map's *neighbouring
	// source file* rather than from the spec, so it restated the drift instead
	// of catching it and made the mismatch look deliberate for as long as it
	// stood. Against Meta templates registered to the spec, a payment receipt
	// would have gone out under the hard-suspension template.
	want := map[string]string{
		"TMPL-001": "fup_warning_80pct",      // FUP 80% warning
		"TMPL-002": "fup_throttled",          // FUP throttle applied
		"TMPL-003": "payment_reminder",       // Renewal reminder (T-7d/3d/1d)
		"TMPL-004": "service_suspended_soft", // Soft suspension
		"TMPL-005": "service_suspended_hard", // Hard suspension
		"TMPL-006": "service_restored",       // Service restored
		"TMPL-007": "payment_received",       // Payment received
		"TMPL-008": "ticket_update",          // Ticket update
	}
	for id, name := range want {
		if got := notifications.TemplateNameFor(id); got != name {
			t.Errorf("TemplateNameFor(%s): want %q, got %q", id, name, got)
		}
	}
}

// TestFR_NOTIF_010_TemplateNameFor_UnknownIDPassesThrough pins the deliberate
// fall-through: a template registered with Meta after this binary shipped must
// still send rather than being blocked by a stale local map.
func TestFR_NOTIF_010_TemplateNameFor_UnknownIDPassesThrough(t *testing.T) {
	if got := notifications.TemplateNameFor("TMPL-999"); got != "TMPL-999" {
		t.Errorf("unknown id should pass through unchanged, got %q", got)
	}
}

// TestFR_NOTIF_010_SendTemplate_UsesApprovedNameOnTheWire is the end of that
// chain: whatever the mapping says must be what actually reaches Meta. A
// correct map paired with a send path that ignored it would still fail in
// production, and only inspecting the request body catches that.
func TestFR_NOTIF_010_SendTemplate_UsesApprovedNameOnTheWire(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.TMPL"}]}`))
	}))
	defer srv.Close()

	db := &stubNotifDB{subscriber: &notifications.Subscriber{ID: 1, MobileNumber: "+919876543210"}}
	c := notifications.NewWhatsAppClient("phone-id", "token", db)
	c.SetBaseURL(srv.URL)

	err := c.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 1,
		ToPhoneE164:  "+919876543210",
		TemplateName: notifications.TemplateNameFor("TMPL-001"),
		TemplateID:   "TMPL-001",
		TriggerEvent: "fup_warning_80pct",
		Variables:    []string{"sub1", "80%"},
	})
	if err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}
	if !strings.Contains(body, `"type":"template"`) {
		t.Error(`request must declare "type":"template" — free-form messages are not permitted to unopted numbers`)
	}
	if !strings.Contains(body, "fup_warning_80pct") {
		t.Errorf("approved template name missing from the request body: %s", body)
	}
	if strings.Contains(body, "TMPL-001") {
		t.Error("the internal template id leaked to Meta; it expects the approved name")
	}
}

// TestFR_NOTIF_009_SendTemplate_LogsFailedDispatch — a provider rejection must
// leave a row behind. Without one, notification_log cannot distinguish "we
// never tried to warn this subscriber" from "we tried and Meta refused", and
// those are very different answers when a customer was suspended without
// warning. This path wrote nothing until 2026-08-11, while the DoD recorded
// the requirement as passing on the strength of a code read.
func TestFR_NOTIF_009_SendTemplate_LogsFailedDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // what an unconfigured deployment gets
	}))
	defer srv.Close()

	db := &stubNotifDB{subscriber: &notifications.Subscriber{ID: 1, MobileNumber: "+919876543210"}}
	c := notifications.NewWhatsAppClient("phone-id", "token", db)
	c.SetBaseURL(srv.URL)

	err := c.SendTemplate(context.Background(), notifications.TemplateMessage{
		SubscriberID: 1,
		ToPhoneE164:  "+919876543210",
		TemplateName: "payment_reminder",
		TemplateID:   "TMPL-003",
		TriggerEvent: "dunning_remind_3d",
	})
	if err == nil {
		t.Fatal("a 401 from the provider must surface as an error")
	}

	if len(db.loggedEntries) != 1 {
		t.Fatalf("want 1 notification_log row for the failed attempt, got %d", len(db.loggedEntries))
	}
	entry := db.loggedEntries[0]
	if entry.DeliveryStatus != "failed" {
		t.Errorf("delivery_status: want \"failed\", got %q", entry.DeliveryStatus)
	}
	if entry.TemplateID != "TMPL-003" || entry.TriggeredByEvent != "dunning_remind_3d" {
		t.Errorf("the row must say which message failed and why it was sent: %+v", entry)
	}
}
