//go:build linux || darwin

package radius

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// readBufferSize reads SO_RCVBUF back off the socket — what the kernel
// actually granted, as opposed to what SetReadBuffer asked for.
//
// Note for anyone comparing this against a sysctl: Linux reports SO_RCVBUF
// as roughly twice the requested value, because the figure includes the
// kernel's own per-socket bookkeeping overhead. applyReadBuffer treats
// "granted >= requested" as success rather than requiring equality for that
// reason.
func readBufferSize(conn *net.UDPConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("radius: raw conn: %w", err)
	}

	var (
		size   int
		optErr error
	)
	if err := raw.Control(func(fd uintptr) {
		size, optErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
	}); err != nil {
		return 0, fmt.Errorf("radius: control raw conn: %w", err)
	}
	if optErr != nil {
		return 0, fmt.Errorf("radius: getsockopt SO_RCVBUF: %w", optErr)
	}
	return size, nil
}
