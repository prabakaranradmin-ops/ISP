//go:build windows

package winservice

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// IsWindowsService reports whether the process was started by the Service
// Control Manager rather than from a console. svc.IsWindowsService's own
// error case (it can fail to inspect the parent process) is treated as "no"
// — falling back to interactive mode is the safe direction, since that path
// only needs a console that responds to Ctrl+C, which is always available;
// guessing "yes" wrongly would hand control to Run and then block forever
// waiting for SCM messages nobody is going to send.
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return is
}

// Run hands control to the Service Control Manager under the given service
// name and blocks until it asks the service to stop. It returns once the
// underlying runFn has returned following that stop request.
func Run(name string, runFn RunFunc) error {
	return svc.Run(name, &handler{name: name, runFn: runFn})
}

// Fatal best-effort logs err to the Windows Event Log. It exists for the
// startup-failure path under a service: there is no console to print to
// (main()'s ordinary fmt.Fprintf(os.Stderr, ...) goes nowhere under SCM),
// and an operator's first move after "the service won't start" is Event
// Viewer. The event source is registered by
// scripts/windows/register_services.ps1 at install time; if that was never
// run, eventlog.Open fails and this quietly does nothing rather than
// compounding one failure with another.
func Fatal(name string, err error) {
	elog, openErr := eventlog.Open(name)
	if openErr != nil {
		return
	}
	defer elog.Close() //nolint:errcheck
	_ = elog.Error(1, fmt.Sprintf("%s: %v", name, err))
}

// handler implements svc.Handler. One instance serves exactly one Run call.
type handler struct {
	name  string
	runFn RunFunc
}

// acceptedCommands are the only SCM requests this service reacts to. Pause
// and continue are deliberately not in the set: neither service has a
// meaningful paused state (an API mid-request or a RADIUS daemon mid-auth
// has nothing sensible to "pause" into), and not accepting them makes the
// SCM grey out those controls instead of sending a request nothing handles.
const acceptedCommands = svc.AcceptStop | svc.AcceptShutdown

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// runFn (cmd/api or cmd/radiusd's run) does its own dependency setup —
	// connecting to PostgreSQL, loading the key store, generating the TLS
	// certificate — before it ever starts serving. Reporting Running only
	// after it signals readiness would need a second channel threaded
	// through every one of those steps for no real benefit: the SCM's
	// start timeout is generous (the default is 30s, wall-clockable via
	// WaitHint below if it ever isn't), and a slow dependency should be
	// visible as a slow start, not hidden behind an extended StartPending.
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.runFn(ctx)
	}()

	s <- svc.Status{State: svc.Running, Accepts: acceptedCommands}

	for {
		select {
		case err := <-errCh:
			// runFn returned on its own, before any stop was requested —
			// a dependency failed (PostgreSQL unreachable, a malformed
			// secret) rather than a normal shutdown. Reported to the event
			// log since nothing else is watching, then treated as a
			// genuine service failure so the SCM's configured recovery
			// actions (restart, if register_services.ps1 set one) engage.
			s <- svc.Status{State: svc.Stopped}
			if err != nil {
				Fatal(h.name, err)
				return false, 1
			}
			return false, 0

		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				s <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: shutdownWaitHintMillis}
				cancel()
				// Waited on here rather than falling through to the loop's
				// top: draining errCh is what actually confirms runFn's
				// graceful shutdown (HTTP Shutdown, worker-pool drain)
				// finished, and Stopped must not be reported to the SCM
				// before it has.
				<-errCh
				s <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}

// shutdownWaitHintMillis tells the SCM how long to wait before concluding
// the service is hung and killing it outright. It has to clear both
// services' own shutdownTimeout (15s, for draining in-flight requests /
// worker tasks) with real margin, since a kill mid-drain is exactly the
// outcome a graceful shutdown exists to avoid.
const shutdownWaitHintMillis = 30000
