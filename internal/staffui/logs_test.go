package staffui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serveLogs(h *Handler, role, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	w := httptest.NewRecorder()
	h.Logs(w, r)
	return w
}

func TestLogsScreen(t *testing.T) {
	now := time.Now()
	code := 503
	msgs := []MessageLogRow{{ID: 1, SubscriberID: 1, Username: "test_user", Channel: "sms", Event: "renewal_reminder",
		SentAt: now, Status: "failed", FailureReason: "DLT template not registered"}}
	hooks := []WebhookLogRow{{ID: 1, Partner: "Acme OTT", URL: "https://acme/hook", EventType: "subscriber.created",
		Status: "failed", Attempts: 3, ResponseStatus: &code, LastError: "timeout", CreatedAt: now}}

	t.Run("roles: owner, NOC, billing, CSR in; technician out", func(t *testing.T) {
		for role, want := range map[string]int{"isp_owner": 200, "noc_engineer": 200, "billing_admin": 200, "csr": 200, "technician": 403} {
			h := &Handler{console: &fakeConsole{}}
			if code := serveLogs(h, role, "/staff/logs").Code; code != want {
				t.Errorf("%s: want %d, got %d", role, want, code)
			}
		}
	})

	t.Run("a failed message shows why", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{messages: msgs}}
		rows := tableBody(serveLogs(h, "csr", "/staff/logs").Body.String())
		if !strings.Contains(rows, "DLT template not registered") || !strings.Contains(rows, "SMS") {
			t.Error("the failure reason and channel must be shown")
		}
	})

	t.Run("filters reach the store; unknown values are dropped and reported", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		serveLogs(h, "csr", "/staff/logs?channel=sms&status=failed&q=+test+")
		if f := fc.gotLog; f == nil || f.Channel != "sms" || f.Status != "failed" || f.Query != "test" || f.To.Sub(f.From) > maxLogRange {
			t.Fatalf("filter: %+v", fc.gotLog)
		}
		body := serveLogs(h, "csr", "/staff/logs?channel=fax&status=lost").Body.String()
		if fc.gotLog.Channel != "" || fc.gotLog.Status != "" || !strings.Contains(body, "Unknown") {
			t.Error("unknown channel/status must not reach the store")
		}
	})

	t.Run("webhooks are the owner's alone, and the tab is hidden from others", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{webhooks: hooks}}
		if code := serveLogs(h, "csr", "/staff/logs?tab=webhooks").Code; code != 403 {
			t.Errorf("CSR on webhooks: want 403, got %d", code)
		}
		if strings.Contains(serveLogs(h, "billing_admin", "/staff/logs").Body.String(), "Partner webhooks") {
			t.Error("the webhook tab must not be offered to billing")
		}
		rows := tableBody(serveLogs(h, "isp_owner", "/staff/logs?tab=webhooks").Body.String())
		if !strings.Contains(rows, "Acme OTT") || !strings.Contains(rows, "503") {
			t.Error("owner should see the partner delivery and its response code")
		}
	})

	t.Run("a store failure is reported", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{err: errors.New("boom")}}
		if !strings.Contains(serveLogs(h, "csr", "/staff/logs").Body.String(), "Could not load the log.") {
			t.Error("failure must be reported")
		}
	})
}

func TestRenderLogs(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	assertRendersOK(t, h, "logs", pageData{Session: testSession(), Data: logsData{Tab: "messages", Channels: MessageChannels, Statuses: MessageStatuses, ShowWebhook: true,
		Messages: []MessageLogRow{{Username: "u", Channel: "push", Status: "suppressed_dnd", SentAt: now}}}})
	assertRendersOK(t, h, "logs", pageData{Session: testSession(), Data: logsData{Tab: "webhooks", Statuses: WebhookStatuses, ShowWebhook: true,
		Webhooks: []WebhookLogRow{{Partner: "p", Status: "delivered", CreatedAt: now, DeliveredAt: &now}}}})
	assertRendersOK(t, h, "logs", pageData{Session: testSession(), Data: logsData{Tab: "messages"}})
}
