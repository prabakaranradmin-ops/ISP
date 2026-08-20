//go:build windows

package winservice

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// drainStatus reads pending status updates off s without blocking once none
// remain, so tests can assert on the sequence Execute reported.
func drainStatus(t *testing.T, s <-chan svc.Status, n int) []svc.Status {
	t.Helper()
	got := make([]svc.Status, 0, n)
	for i := 0; i < n; i++ {
		select {
		case st := <-s:
			got = append(got, st)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for status update %d/%d (got %d so far)", i+1, n, len(got))
		}
	}
	return got
}

func TestExecute_StopRequestDrainsGracefullyThenReportsStopped(t *testing.T) {
	var cancelled bool
	runStarted := make(chan struct{})
	runFn := func(ctx context.Context) error {
		close(runStarted)
		<-ctx.Done()
		cancelled = true
		return nil
	}

	h := &handler{name: "test-svc", runFn: runFn}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)

	done := make(chan struct {
		specific bool
		code     uint32
	})
	go func() {
		specific, code := h.Execute(nil, reqCh, statusCh)
		done <- struct {
			specific bool
			code     uint32
		}{specific, code}
	}()

	<-runStarted

	// StartPending, then Running with the two commands this service reacts to.
	got := drainStatus(t, statusCh, 2)
	if got[0].State != svc.StartPending {
		t.Errorf("first status = %v, want StartPending", got[0].State)
	}
	if got[1].State != svc.Running || got[1].Accepts != acceptedCommands {
		t.Errorf("second status = %+v, want Running with Accepts=%v", got[1], acceptedCommands)
	}

	reqCh <- svc.ChangeRequest{Cmd: svc.Stop}

	got = drainStatus(t, statusCh, 2)
	if got[0].State != svc.StopPending {
		t.Errorf("third status = %v, want StopPending", got[0].State)
	}
	if got[1].State != svc.Stopped {
		t.Errorf("fourth status = %v, want Stopped", got[1].State)
	}

	result := <-done
	if result.specific || result.code != 0 {
		t.Errorf("Execute() = (%v, %d), want (false, 0) on a requested stop", result.specific, result.code)
	}
	if !cancelled {
		t.Error("runFn's context was never cancelled on Stop")
	}
}

func TestExecute_RunFnFailureIsReportedAsAServiceFailure(t *testing.T) {
	wantErr := errors.New("connect to PostgreSQL: dial tcp: connection refused")
	h := &handler{name: "test-svc", runFn: func(ctx context.Context) error {
		return wantErr
	}}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)

	specific, code := h.Execute(nil, reqCh, statusCh)

	// A dependency failure at startup is not the "asked to stop" path, so
	// the SCM must see a nonzero exit — that's what lets a configured
	// restart-on-failure recovery action engage.
	if specific || code != 1 {
		t.Errorf("Execute() = (%v, %d), want (false, 1) when runFn fails unprompted", specific, code)
	}

	got := drainStatus(t, statusCh, 3)
	if got[len(got)-1].State != svc.Stopped {
		t.Errorf("final status = %v, want Stopped", got[len(got)-1].State)
	}
}

func TestExecute_InterrogateEchoesCurrentStatus(t *testing.T) {
	runFn := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	h := &handler{name: "test-svc", runFn: runFn}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)

	go h.Execute(nil, reqCh, statusCh) //nolint:errcheck

	drainStatus(t, statusCh, 2) // StartPending, Running

	reqCh <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running, Accepts: acceptedCommands}}

	got := drainStatus(t, statusCh, 1)
	if got[0].State != svc.Running {
		t.Errorf("Interrogate echoed %v, want the Running status handed to it", got[0].State)
	}

	reqCh <- svc.ChangeRequest{Cmd: svc.Stop}
	drainStatus(t, statusCh, 2) // StopPending, Stopped
}
