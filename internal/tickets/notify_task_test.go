package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

type stubNotifier struct {
	subscriberID int
	templateID   string
	triggerEvent string
	vars         []string
	err          error
	calls        int
}

func (s *stubNotifier) Notify(_ context.Context, subscriberID int, templateID, triggerEvent string, vars []string, channels ...string) error {
	s.calls++
	s.subscriberID = subscriberID
	s.templateID = templateID
	s.triggerEvent = triggerEvent
	s.vars = vars
	return s.err
}

func TestUpdateHandler_ProcessTask_DispatchesWithTemplateAndVars(t *testing.T) {
	notifier := &stubNotifier{}
	h := NewUpdateHandler(notifier)

	payload, err := json.Marshal(UpdatePayload{
		SubscriberID: 42,
		Username:     "vinoth",
		TicketID:     7,
		Status:       "resolved",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeTicketUpdate, payload)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if notifier.calls != 1 {
		t.Fatalf("expected 1 Notify call, got %d", notifier.calls)
	}
	if notifier.subscriberID != 42 {
		t.Errorf("subscriber id = %d, want 42", notifier.subscriberID)
	}
	if notifier.templateID != TemplateTicketUpdate {
		t.Errorf("template id = %q, want %q", notifier.templateID, TemplateTicketUpdate)
	}
	wantVars := []string{"vinoth", "7", "resolved"}
	if len(notifier.vars) != len(wantVars) {
		t.Fatalf("vars = %v, want %v", notifier.vars, wantVars)
	}
	for i, v := range wantVars {
		if notifier.vars[i] != v {
			t.Errorf("vars[%d] = %q, want %q", i, notifier.vars[i], v)
		}
	}
}

// A malformed payload must not retry: it will never become valid, and Asynq
// only skips retry when the handler wraps jobqueue.SkipRetry into the error it
// returns.
func TestUpdateHandler_ProcessTask_MalformedPayloadSkipsRetry(t *testing.T) {
	h := NewUpdateHandler(&stubNotifier{})

	err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeTicketUpdate, []byte("{not json")))
	if err == nil {
		t.Fatal("expected an error for malformed payload")
	}
	if !errors.Is(err, jobqueue.SkipRetry) {
		t.Errorf("expected error to wrap jobqueue.SkipRetry, got: %v", err)
	}
}

func TestUpdateHandler_ProcessTask_NilNotifierErrors(t *testing.T) {
	h := NewUpdateHandler(nil)

	payload, _ := json.Marshal(UpdatePayload{SubscriberID: 1, TicketID: 1, Status: "open"})
	err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeTicketUpdate, payload))
	if err == nil {
		t.Fatal("expected an error when notifier is not configured")
	}
}

func TestUpdateHandler_ProcessTask_NotifierErrorPropagates(t *testing.T) {
	wantErr := errors.New("dispatch failed")
	h := NewUpdateHandler(&stubNotifier{err: wantErr})

	payload, _ := json.Marshal(UpdatePayload{SubscriberID: 1, TicketID: 1, Status: "open"})
	err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeTicketUpdate, payload))
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected error wrapping %v, got %v", wantErr, err)
	}
}
