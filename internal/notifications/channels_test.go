package notifications_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// Email and push channel tests — FR-NOTIF-012..013 | MDS §4.17.
//
// The behaviour worth pinning is what happens when a subscriber simply
// cannot be reached on a channel: no email address, no registered device.
// That is a normal state for most subscribers, and treating it as a delivery
// error would push it into Asynq's retry-and-dead-letter path for something
// no retry can fix.

// stubEmail records SendEmail calls.
type stubEmail struct {
	calls   int
	lastTo  string
	lastSub string
	lastMsg string
	err     error
}

func (s *stubEmail) SendEmail(_ context.Context, to, subject, body string) error {
	s.calls++
	s.lastTo, s.lastSub, s.lastMsg = to, subject, body
	return s.err
}

// stubPush records SendPush calls.
type stubPush struct {
	calls      int
	lastTokens []string
	err        error
}

func (s *stubPush) SendPush(_ context.Context, tokens []string, _, _ string) error {
	s.calls++
	s.lastTokens = tokens
	return s.err
}

func TestFR_NOTIF_012_EmailIsSentToTheSubscribersAddress(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111", Email: "ravi@example.com",
	}}
	email := &stubEmail{}
	d := notifications.NewDispatcher(db, nil, nil)
	d.SetEmailSender(email)

	err := d.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1, Channel: "email", Class: "transactional",
		Subject: "Your invoice", Variables: []string{"Body text"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if email.calls != 1 {
		t.Fatalf("want 1 email sent, got %d", email.calls)
	}
	if email.lastTo != "ravi@example.com" {
		t.Errorf("to = %q, want the subscriber's address", email.lastTo)
	}
	if email.lastSub != "Your invoice" {
		t.Errorf("subject = %q", email.lastSub)
	}
	// FR-NOTIF-009: every outbound notification creates a log record.
	if len(db.loggedEntries) != 1 || db.loggedEntries[0].DeliveryStatus != "sent" {
		t.Errorf("want one 'sent' log entry, got %+v", db.loggedEntries)
	}
}

// TestFR_NOTIF_012_SubscriberWithNoEmailIsLoggedNotErrored is the negative
// control: an unreachable subscriber must be recorded and shrugged off, not
// retried forever.
func TestFR_NOTIF_012_SubscriberWithNoEmailIsLoggedNotErrored(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111", Email: "", // never supplied one
	}}
	email := &stubEmail{}
	d := notifications.NewDispatcher(db, nil, nil)
	d.SetEmailSender(email)

	err := d.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1, Channel: "email", Class: "transactional", Subject: "Hi",
	})
	if err != nil {
		t.Fatalf("an unreachable subscriber must not be an error (it would retry forever): %v", err)
	}
	if email.calls != 0 {
		t.Error("nothing should have been sent")
	}
	if len(db.loggedEntries) != 1 {
		t.Fatalf("want the failure logged, got %+v", db.loggedEntries)
	}
	if db.loggedEntries[0].DeliveryStatus != "failed" {
		t.Errorf("delivery_status = %q, want failed", db.loggedEntries[0].DeliveryStatus)
	}
	if db.loggedEntries[0].FailureReason == "" {
		t.Error("the log must say why it could not be delivered")
	}
}

func TestFR_NOTIF_013_PushGoesToEveryRegisteredDevice(t *testing.T) {
	db := &stubNotifDB{
		subscriber: &notifications.Subscriber{ID: 1, MobileNumber: "+919876500111"},
		pushTokens: []string{"tok-phone", "tok-tablet"},
	}
	push := &stubPush{}
	d := notifications.NewDispatcher(db, nil, nil)
	d.SetPushSender(push)

	err := d.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1, Channel: "push", Class: "transactional",
		Subject: "Plan expiring", Variables: []string{"Renew today"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if push.calls != 1 {
		t.Fatalf("want 1 push call, got %d", push.calls)
	}
	// One call carrying both tokens, not two calls: one notification, one
	// delivery outcome, one log row.
	if len(push.lastTokens) != 2 {
		t.Errorf("want both device tokens in one call, got %v", push.lastTokens)
	}
}

func TestFR_NOTIF_013_SubscriberWithNoDeviceIsLoggedNotErrored(t *testing.T) {
	db := &stubNotifDB{
		subscriber: &notifications.Subscriber{ID: 1, MobileNumber: "+919876500111"},
		pushTokens: nil, // never installed the app
	}
	push := &stubPush{}
	d := notifications.NewDispatcher(db, nil, nil)
	d.SetPushSender(push)

	err := d.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1, Channel: "push", Class: "transactional", Subject: "Hi",
	})
	if err != nil {
		t.Fatalf("a subscriber with no device must not be an error: %v", err)
	}
	if push.calls != 0 {
		t.Error("nothing should have been sent")
	}
	if len(db.loggedEntries) != 1 || db.loggedEntries[0].DeliveryStatus != "failed" {
		t.Errorf("want one logged failure, got %+v", db.loggedEntries)
	}
}

// TestFR_NOTIF_008_DNDSuppressesMarketingOnEveryChannel: the suppression
// rule must apply to the new channels too, or an opted-out subscriber would
// simply be reached by email instead.
func TestFR_NOTIF_008_DNDSuppressesMarketingOnEveryChannel(t *testing.T) {
	for _, channel := range []string{"email", "push"} {
		t.Run(channel, func(t *testing.T) {
			db := &stubNotifDB{
				subscriber: &notifications.Subscriber{
					ID: 1, MobileNumber: "+919876500111",
					Email: "ravi@example.com", DndOptOut: true,
				},
				pushTokens: []string{"tok"},
			}
			email, push := &stubEmail{}, &stubPush{}
			d := notifications.NewDispatcher(db, nil, nil)
			d.SetEmailSender(email)
			d.SetPushSender(push)

			err := d.Dispatch(context.Background(), notifications.NotificationTask{
				SubscriberID: 1, Channel: channel, Class: "marketing", Subject: "Offer",
			})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if email.calls != 0 || push.calls != 0 {
				t.Error("an opted-out subscriber must not be reached on any channel")
			}
			if len(db.loggedEntries) != 1 || db.loggedEntries[0].DeliveryStatus != "suppressed_dnd" {
				t.Errorf("want a suppressed_dnd log entry, got %+v", db.loggedEntries)
			}
		})
	}
}

// TestFR_NOTIF_008_TransactionalStillReachesOptedOutSubscribers is the
// positive control: DND must not suppress a suspension notice.
func TestFR_NOTIF_008_TransactionalStillReachesOptedOutSubscribers(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111", Email: "ravi@example.com", DndOptOut: true,
	}}
	email := &stubEmail{}
	d := notifications.NewDispatcher(db, nil, nil)
	d.SetEmailSender(email)

	err := d.Dispatch(context.Background(), notifications.NotificationTask{
		SubscriberID: 1, Channel: "email", Class: "transactional", Subject: "Service suspended",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if email.calls != 1 {
		t.Error("a transactional notice must still reach an opted-out subscriber")
	}
}

// TestFR_NOTIF_009_EverySentChannelWritesALogRecord: SMS used to return
// without logging, so the audit trail the FR promises was incomplete for
// that channel. This pins all three non-WhatsApp channels (WhatsApp writes
// its own, keyed by provider message id, in the client).
func TestFR_NOTIF_009_EverySentChannelWritesALogRecord(t *testing.T) {
	cases := []struct {
		channel string
		attach  func(*notifications.Dispatcher)
	}{
		{"sms", func(d *notifications.Dispatcher) {}},
		{"email", func(d *notifications.Dispatcher) { d.SetEmailSender(&stubEmail{}) }},
		{"push", func(d *notifications.Dispatcher) { d.SetPushSender(&stubPush{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.channel, func(t *testing.T) {
			db := &stubNotifDB{
				subscriber: &notifications.Subscriber{
					ID: 1, MobileNumber: "+919876500111", Email: "ravi@example.com",
				},
				pushTokens: []string{"tok"},
			}
			d := notifications.NewDispatcher(db, nil, &stubSMS{})
			tc.attach(d)

			err := d.Dispatch(context.Background(), notifications.NotificationTask{
				SubscriberID: 1, Channel: tc.channel, Class: "transactional",
				Subject: "Hi", Variables: []string{"Body"},
			})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if len(db.loggedEntries) != 1 {
				t.Fatalf("want exactly 1 log record, got %d", len(db.loggedEntries))
			}
			if db.loggedEntries[0].Channel != tc.channel {
				t.Errorf("logged channel = %q, want %q", db.loggedEntries[0].Channel, tc.channel)
			}
			if db.loggedEntries[0].DeliveryStatus != "sent" {
				t.Errorf("delivery_status = %q, want sent", db.loggedEntries[0].DeliveryStatus)
			}
		})
	}
}

func TestUnconfiguredChannelsReportClearly(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{ID: 1, Email: "x@y.com"}}
	d := notifications.NewDispatcher(db, nil, nil) // no email or push attached

	for _, channel := range []string{"email", "push"} {
		err := d.Dispatch(context.Background(), notifications.NotificationTask{
			SubscriberID: 1, Channel: channel, Class: "transactional",
		})
		if err == nil {
			t.Errorf("%s: want an error when the channel is not configured", channel)
		}
	}
}

// ── Client-level checks ─────────────────────────────────────────────────────

func TestSMTPClient_UnconfiguredIsReportedNotPanicked(t *testing.T) {
	c := notifications.NewSMTPClient(notifications.SMTPConfig{})
	if c.Configured() {
		t.Error("a client with no host must report itself unconfigured")
	}
	err := c.SendEmail(context.Background(), "a@b.com", "s", "b")
	if !errors.Is(err, notifications.ErrEmailNotConfigured) {
		t.Errorf("want ErrEmailNotConfigured, got %v", err)
	}
}

func TestSMTPClient_RejectsMalformedRecipient(t *testing.T) {
	c := notifications.NewSMTPClient(notifications.SMTPConfig{Host: "localhost", From: "a@b.com"})
	if err := c.SendEmail(context.Background(), "not-an-address", "s", "b"); err == nil {
		t.Error("want an error for a recipient with no @")
	}
}

func TestOneSignalClient_UnconfiguredIsReported(t *testing.T) {
	c := notifications.NewOneSignalClient("", "")
	if c.Configured() {
		t.Error("a client with no credentials must report itself unconfigured")
	}
	if err := c.SendPush(context.Background(), []string{"tok"}, "t", "b"); !errors.Is(err, notifications.ErrPushNotConfigured) {
		t.Errorf("want ErrPushNotConfigured, got %v", err)
	}
}

func TestOneSignalClient_EmptyTokenListIsRejected(t *testing.T) {
	c := notifications.NewOneSignalClient("app", "key")
	if err := c.SendPush(context.Background(), nil, "t", "b"); err == nil {
		t.Error("want an error rather than sending a malformed request with no recipients")
	}
}

// TestAnnouncementTaskID_ScopedPerRecipientAndChannel pins the fan-out
// idempotency key. Without the channel in it, a subscriber targeted on both
// SMS and email would get only one of the two.
func TestAnnouncementTaskID_ScopedPerRecipientAndChannel(t *testing.T) {
	base := notifications.AnnouncementTaskID(7, 42, "sms")
	if base != notifications.AnnouncementTaskID(7, 42, "sms") {
		t.Error("the same announcement, subscriber and channel must produce a stable id")
	}
	for name, other := range map[string]string{
		"different channel":      notifications.AnnouncementTaskID(7, 42, "email"),
		"different subscriber":   notifications.AnnouncementTaskID(7, 43, "sms"),
		"different announcement": notifications.AnnouncementTaskID(8, 42, "sms"),
	} {
		if base == other {
			t.Errorf("%s must produce a different id", name)
		}
	}
}

func TestValidChannelExcludesPortal(t *testing.T) {
	for _, c := range []string{"whatsapp", "sms", "email", "push"} {
		if !notifications.ValidChannel(c) {
			t.Errorf("%s must be a valid announcement channel", c)
		}
	}
	// A banner is displayed, not transmitted — it has no delivery status and
	// must not become a notification_log row.
	if notifications.ValidChannel("portal") {
		t.Error("portal must not be a dispatched channel; it is the show_in_portal flag")
	}
}

// TestEmailSubjectCannotInjectHeaders: a subject carrying CRLF could
// otherwise smuggle extra headers (a Bcc, say) into the outgoing message.
func TestEmailSubjectCannotInjectHeaders(t *testing.T) {
	msg := notifications.BuildMessageForTest("from@isp.com", "to@x.com",
		"Hello\r\nBcc: attacker@evil.com", "body")

	if strings.Contains(msg, "Bcc:") && strings.Count(msg, "\r\n\r\n") > 1 {
		t.Error("a CRLF in the subject must not be able to introduce a new header")
	}
	subjectLine := ""
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subjectLine = line
		}
	}
	if strings.Contains(subjectLine, "\n") || strings.Contains(subjectLine, "\r") {
		t.Errorf("the subject header still contains a line break: %q", subjectLine)
	}
	if !strings.Contains(subjectLine, "Bcc: attacker@evil.com") {
		t.Errorf("the injected text should survive as inert subject text, got %q", subjectLine)
	}
}

// ── Multi-channel Notify ────────────────────────────────────────────────
//
// Notify used to hardcode the WhatsApp channel, so every requirement that
// asks for "WhatsApp + SMS" (FR-NOTIF-001 through 006) delivered one of the
// two. It now takes channel targets, and what needs pinning is the failure
// rule, which is the part that is easy to get subtly wrong.

// recordingSMS records where a message went and can be made to fail. The
// existing stubSMS counts calls only, which cannot distinguish "attempted
// and failed" from "never attempted" — the distinction these tests turn on.
type recordingSMS struct {
	calls  int
	lastTo string
	err    error
}

func (s *recordingSMS) SendSMS(_ context.Context, to, _ string) error {
	s.calls++
	s.lastTo = to
	return s.err
}

// The default has to stay WhatsApp-only. Every existing caller relies on it,
// and a default that fanned out would hand an operator a per-message SMS bill
// for handlers nobody had reconsidered.
func TestNotify_DefaultsToWhatsAppOnly(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111",
	}}
	sms := &recordingSMS{}
	d := notifications.NewDispatcher(db, nil, sms)

	// No WhatsApp client configured, so this errors — the point is only that
	// SMS was never attempted.
	_ = d.Notify(context.Background(), 1, "TMPL-005", "dunning_hard_suspended", []string{"x"})
	if sms.calls != 0 {
		t.Errorf("SMS sent %d times with no channels named; the default must stay WhatsApp-only", sms.calls)
	}
}

func TestNotify_SendsOnEveryNamedChannel(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111",
	}}
	sms := &recordingSMS{}
	d := notifications.NewDispatcher(db, nil, sms)

	if err := d.Notify(context.Background(), 1, "TMPL-006", "service_restored",
		[]string{"restored"}, notifications.ChannelSMS); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if sms.calls != 1 {
		t.Errorf("SMS calls: want 1, got %d", sms.calls)
	}
	if sms.lastTo != "+919876500111" {
		t.Errorf("SMS destination: got %q", sms.lastTo)
	}
}

// The rule that matters. A queued task retries on error, so returning one
// because a secondary channel failed would redeliver the channel that had
// already arrived — the subscriber gets the same WhatsApp message twice
// because their SMS gateway hiccuped.
func TestNotify_OneChannelFailingDoesNotFailTheWhole(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111", Email: "ravi@example.com",
	}}
	sms := &recordingSMS{err: errors.New("gateway 502")}
	email := &stubEmail{}
	d := notifications.NewDispatcher(db, nil, sms)
	d.SetEmailSender(email)

	err := d.Notify(context.Background(), 1, "TMPL-003", "dunning_remind_7d",
		[]string{"reminder"}, notifications.ChannelSMS, notifications.ChannelEmail)
	if err != nil {
		t.Errorf("one channel failing must not fail the notification, got: %v", err)
	}
	if email.calls != 1 {
		t.Errorf("the surviving channel must still be attempted: email calls = %d", email.calls)
	}
	if sms.calls != 1 {
		t.Errorf("the failing channel should have been attempted once, got %d", sms.calls)
	}
}

// If nothing got through, the caller has to know: that is a real delivery
// failure and the queue should retry it.
func TestNotify_AllChannelsFailingIsAnError(t *testing.T) {
	db := &stubNotifDB{subscriber: &notifications.Subscriber{
		ID: 1, MobileNumber: "+919876500111",
	}}
	sms := &recordingSMS{err: errors.New("gateway 502")}
	d := notifications.NewDispatcher(db, nil, sms)

	if err := d.Notify(context.Background(), 1, "TMPL-005", "dunning_hard_suspended",
		[]string{"x"}, notifications.ChannelSMS); err == nil {
		t.Error("want an error when no channel succeeded, got nil — the task would be marked done undelivered")
	} else if !strings.Contains(err.Error(), "sms") {
		t.Errorf("the error should name the channel that failed, got: %v", err)
	}
}
