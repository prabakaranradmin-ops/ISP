//go:build !linux && !darwin

package radius

import (
	"errors"
	"net"
)

// readBufferSize has no portable implementation off Linux/macOS.
//
// The native Windows install (installer/, scripts/windows/) builds this
// package, so this file exists to keep it compiling rather than because the
// readback is unavailable in principle — Winsock has SO_RCVBUF too. It is
// unimplemented because the tuning it reports on is a Linux sysctl
// (net.core.rmem_max) that has no Windows equivalent worth surfacing:
// Windows does not clamp SO_RCVBUF the same way, so the "your request was
// silently capped" warning this readback exists to produce would never fire.
//
// applyReadBuffer treats the error as "size unknown" and logs at debug, so
// the buffer is still set on Windows; only the confirmation is skipped.
func readBufferSize(*net.UDPConn) (int, error) {
	return 0, errors.New("radius: socket buffer readback is not implemented on this platform")
}
