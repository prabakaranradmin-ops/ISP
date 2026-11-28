//go:build integration

// What an announcement records, and what the operator is told, when its
// messages cannot be queued — FR-ANN-001, and the console's bulk "Notify
// these" (SendBulkNotification), which had no test at all.
//
// The defect found writing them: a broadcast whose every enqueue failed — the
// queue down — was recorded as 'sent' and returned success. The console then
// said "Sent to 50 subscribers: 0 messages queued for delivery" in green, the
// bulk action said "sent to 3 of 3", and the announcement could not be sent
// again because it was no longer a draft. Nobody had received anything.
package api_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// flakyQueue fails every second enqueue, as a queue under pressure does.
type flakyQueue struct {
	mu    sync.Mutex
	calls int
	ok    int
}

func (q *flakyQueue) Enqueue(*jobqueue.Task, ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls++
	if q.calls%2 == 0 {
		return nil, errors.New("queue: timeout")
	}
	q.ok++
	return &jobqueue.TaskInfo{ID: 1}, nil
}

// conflictQueue answers every enqueue as already queued: a retry that finds
// its tasks in place has delivered nothing new and lost nothing.
type conflictQueue struct{}

func (conflictQueue) Enqueue(*jobqueue.Task, ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	return nil, jobqueue.ErrTaskIDConflict
}

func annHandler(anns *stubAnnouncements, tasks api.TaskEnqueuer) (*api.Handler, *http.ServeMux) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Announcements: anns, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return h, mux
}

func draft(anns *stubAnnouncements) int {
	return anns.seed(notifications.Announcement{
		Title: "Planned maintenance", Body: "Tonight 1-3am", Channels: []string{"sms", "whatsapp"},
	}).ID
}

func TestFR_ANN_001_ABroadcastNothingCouldBeQueuedForIsAFailure(t *testing.T) {
	anns := newStubAnnouncements()
	anns.segment = []int{11, 12, 13}
	id := draft(anns)
	_, mux := annHandler(anns, &stubTaskEnqueuer{err: errors.New("redis: connection refused")})

	rec := httptestJSON(t, mux, http.MethodPost, "/api/v1/announcements/"+itoa(id)+"/send", "isp_owner", "")
	if rec.Code == http.StatusAccepted {
		t.Fatalf("a broadcast that queued nothing was reported as sent: %s", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "ERR_QUEUE_UNAVAILABLE") {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
	if got := anns.byID[id].Status; got != notifications.AnnouncementFailed {
		t.Errorf("recorded as %q; nobody received it, so it is failed", got)
	}
	if strings.Contains(rec.Body.String(), "redis") {
		t.Error("the queue's error reached the client")
	}
}

func TestFR_ANN_001_APartlyQueuedBroadcastSaysHowManyFailed(t *testing.T) {
	anns := newStubAnnouncements()
	anns.segment = []int{11, 12}
	id := draft(anns)
	q := &flakyQueue{}
	_, mux := annHandler(anns, q)

	rec := httptestJSON(t, mux, http.MethodPost, "/api/v1/announcements/"+itoa(id)+"/send", "isp_owner", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a partly queued broadcast: %d %s", rec.Code, rec.Body.String())
	}
	// Two recipients, two channels: four messages, two of which failed.
	for _, want := range []string{`"tasks_enqueued":2`, `"tasks_failed":2`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %s in %s", want, rec.Body.String())
		}
	}
	if a := anns.byID[id]; a.Status != notifications.AnnouncementSent || a.RecipientCount != 2 {
		t.Errorf("recorded as %s/%d", a.Status, a.RecipientCount)
	}
}

// Messages already queued by an earlier attempt are not a failure.
func TestFR_ANN_001_AlreadyQueuedMessagesAreNotFailures(t *testing.T) {
	anns := newStubAnnouncements()
	anns.segment = []int{11}
	id := draft(anns)
	h, _ := annHandler(anns, conflictQueue{})
	res, err := h.BroadcastAnnouncement(context.Background(), id)
	if err != nil || res.TasksFailed != 0 {
		t.Errorf("conflicts counted as failures: %+v %v", res, err)
	}
}

// A portal-only announcement queues nothing and that is correct, not a
// failure: there were no messages to queue.
func TestFR_ANN_001_APortalOnlyBroadcastIsNotAQueueFailure(t *testing.T) {
	anns := newStubAnnouncements()
	anns.segment = []int{11, 12}
	id := anns.seed(notifications.Announcement{Title: "x", Body: "y", ShowInPortal: true}).ID
	h, _ := annHandler(anns, &stubTaskEnqueuer{err: errors.New("down")})
	if res, err := h.BroadcastAnnouncement(context.Background(), id); err != nil || res.TasksEnqueued != 0 {
		t.Errorf("portal-only: %+v %v", res, err)
	}
}

// ── The console's bulk "Notify these" ──────────────────────────────────────

func TestSendBulkNotificationNamesItsAudienceAndSends(t *testing.T) {
	anns := newStubAnnouncements()
	anns.segment = []int{4, 9}
	tasks := &stubTaskEnqueuer{}
	h, _ := annHandler(anns, tasks)

	n, err := h.SendBulkNotification(context.Background(), "csr_meena", []int{4, 9},
		"Your area", "Fibre cut repaired", []string{"sms"}, true)
	if err != nil || n != 2 {
		t.Fatalf("got %d %v", n, err)
	}
	a := anns.byID[1]
	if a.Class != "transactional" || a.CreatedBy != "csr_meena" || len(a.SubscriberIDs) != 2 || !a.ShowInPortal {
		t.Errorf("created %+v; a hand-picked notice is transactional, so DND does not swallow it", a)
	}
	if a.Status != notifications.AnnouncementSent || a.RecipientCount != 2 {
		t.Errorf("recorded as %s/%d", a.Status, a.RecipientCount)
	}
	if len(tasks.snapshot()) != 2 {
		t.Errorf("queued %d", len(tasks.snapshot()))
	}
}

func TestSendBulkNotificationFailures(t *testing.T) {
	// The queue down: nothing delivered, recorded failed, an error returned.
	anns := newStubAnnouncements()
	anns.segment = []int{4}
	h, _ := annHandler(anns, &stubTaskEnqueuer{err: errors.New("down")})
	if _, err := h.SendBulkNotification(context.Background(), "x", []int{4}, "t", "b", []string{"sms"}, false); err == nil {
		t.Error("a bulk notice that queued nothing returned success")
	}
	if anns.byID[1].Status != notifications.AnnouncementFailed {
		t.Errorf("recorded as %q", anns.byID[1].Status)
	}

	// The recipients cannot be resolved: failed, not left in 'sending'.
	seg := &stubAnnouncementsSegmentDown{stubAnnouncements: newStubAnnouncements()}
	h3 := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Announcements: seg, Tasks: &stubTaskEnqueuer{}})
	if _, err := h3.SendBulkNotification(context.Background(), "x", []int{4}, "t", "b", []string{"sms"}, false); err == nil {
		t.Error("an unresolvable audience returned success")
	}
	if seg.byID[1].Status != notifications.AnnouncementFailed {
		t.Errorf("an unresolvable audience left the notice %q", seg.byID[1].Status)
	}

	// Not configured: an error, not a panic.
	bare := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})})
	if _, err := bare.SendBulkNotification(context.Background(), "x", []int{4}, "t", "b", []string{"sms"}, false); err == nil {
		t.Error("an unconfigured deployment returned success")
	}
}

type stubAnnouncementsSegmentDown struct{ *stubAnnouncements }

func (s *stubAnnouncementsSegmentDown) ListSegmentSubscriberIDs(context.Context, int, *int, *int, *string, *int) ([]int, error) {
	return nil, errors.New("db: segment query failed")
}

func itoa(n int) string { return strconv.Itoa(n) }
