package billing_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// channelRecordingNotifier captures which channels a handler asked for.
type channelRecordingNotifier struct {
	channels []string
	template string
}

func (n *channelRecordingNotifier) Notify(_ context.Context, _ int, templateID, _ string, _ []string, channels ...string) error {
	n.template = templateID
	n.channels = channels
	return nil
}

// A handler with no policy set must stay WhatsApp-only. This is the property
// that keeps an unreviewed handler from silently acquiring a per-message SMS
// bill on the next deploy.
func TestDunningNoticeHandler_WithoutAPolicySendsWhatsAppOnly(t *testing.T) {
	n := &channelRecordingNotifier{}
	h := billing.NewDunningNoticeHandler(n)

	payload, err := json.Marshal(billing.DunningNoticePayload{
		SubscriberID: 1, Username: "sub", State: billing.DunningHardSuspended,
		TemplateID: billing.TemplateHardSuspended,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.ProcessTask(context.Background(),
		jobqueue.NewTask(billing.TaskTypeDunningNotice, payload)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if len(n.channels) != 0 {
		t.Errorf("no policy set should mean no channels named (Notify then defaults to "+
			"WhatsApp), got %v", n.channels)
	}
}

// The policy is consulted per stage, not once per handler: the whole point of
// the operator's choice is that a suspension costs an SMS and a T-7d reminder
// does not.
func TestDunningNoticeHandler_ChannelsVaryByStage(t *testing.T) {
	h := func(state billing.DunningState) []string {
		n := &channelRecordingNotifier{}
		handler := billing.NewDunningNoticeHandler(n)
		handler.SetChannelPolicy(func(s billing.DunningState) []string {
			if s == billing.DunningHardSuspended {
				return []string{"whatsapp", "sms"}
			}
			return []string{"whatsapp"}
		})
		payload, err := json.Marshal(billing.DunningNoticePayload{
			SubscriberID: 1, Username: "sub", State: state, TemplateID: "TMPL-003",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := handler.ProcessTask(context.Background(),
			jobqueue.NewTask(billing.TaskTypeDunningNotice, payload)); err != nil {
			t.Fatalf("ProcessTask: %v", err)
		}
		return n.channels
	}

	if got := h(billing.DunningRemind7d); len(got) != 1 {
		t.Errorf("remind_7d: want 1 channel, got %v", got)
	}
	if got := h(billing.DunningHardSuspended); len(got) != 2 {
		t.Errorf("hard_suspended: want 2 channels, got %v — a suspension notice sent only "+
			"over WhatsApp reaches nobody who is actually suspended", got)
	}
}

// The restoration notice is the one that most needs SMS, and it must carry
// the restored template rather than the receipt one.
func TestServiceRestoredHandler_UsesItsOwnTemplateAndChannels(t *testing.T) {
	n := &channelRecordingNotifier{}
	h := billing.NewServiceRestoredHandler(n)
	h.SetChannels("whatsapp", "sms")

	payload, err := json.Marshal(billing.ServiceRestoredPayload{
		SubscriberID: 1, Username: "sub", PlanName: "Basic", ValidUntil: "2026-10-05",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.ProcessTask(context.Background(),
		jobqueue.NewTask(billing.TaskTypeServiceRestored, payload)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	if n.template != billing.TemplateServiceRestored {
		t.Errorf("template: got %q, want %q (TMPL-006)", n.template, billing.TemplateServiceRestored)
	}
	if len(n.channels) != 2 {
		t.Errorf("channels: got %v, want whatsapp+sms", n.channels)
	}
}

// A receipt must not claim restoration. The template is the payment one
// whether or not the subscriber happened to be suspended when they paid.
func TestPaymentReceiptHandler_NeverSendsTheRestoredTemplate(t *testing.T) {
	for _, wasSuspended := range []bool{false, true} {
		n := &channelRecordingNotifier{}
		h := billing.NewPaymentReceiptHandler(n)

		payload, err := json.Marshal(billing.PaymentReceiptPayload{
			SubscriberID: 1, Username: "sub", Amount: "750.00",
			NewBalance: "750.00", WasSuspended: wasSuspended,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := h.ProcessTask(context.Background(),
			jobqueue.NewTask(billing.TaskTypePaymentReceipt, payload)); err != nil {
			t.Fatalf("ProcessTask: %v", err)
		}
		if n.template != billing.TemplatePaymentReceived {
			t.Errorf("was_suspended=%v: template %q, want %q — the webhook only moves money, "+
				"and claiming restoration here tells a still-suspended subscriber they are back on",
				wasSuspended, n.template, billing.TemplatePaymentReceived)
		}
	}
}
