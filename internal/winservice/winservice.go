// Package winservice lets api_service and aaa_core_daemon run either as an
// ordinary console process (Docker, Linux, a developer's shell) or as a
// registered Windows service, from the same run(ctx) entry point.
//
// The two modes differ in exactly one place: what cancels ctx. Interactively
// that is Ctrl+C or SIGTERM, handled by signal.NotifyContext as it always
// was. Under the Service Control Manager it is a SERVICE_CONTROL_STOP or
// SERVICE_CONTROL_SHUTDOWN request, which arrives on a channel rather than
// as a signal — Windows has no SIGTERM. Run bridges that channel to the same
// context.CancelFunc, so everything downstream of ctx (the HTTP servers'
// graceful Shutdown, the RADIUS daemon's worker-pool drain) behaves
// identically either way and needed no changes to run itself.
//
// IsWindowsService is the switch a main() uses to pick a mode; see
// winservice_windows.go and winservice_other.go for the two implementations
// building this package provides depending on GOOS.
package winservice

import "context"

// RunFunc is the shape both cmd/api and cmd/radiusd's run() already had
// (minus the ctx they used to build internally via signal.NotifyContext).
// It must return once ctx is cancelled.
type RunFunc func(ctx context.Context) error
