//go:build !darwin && !linux

package daemon

import (
	"errors"
	"net"
	"runtime"
)

// peerOK refuses every connection: this system cannot say which user is on the other end of
// the socket, so allowing one would let any local account drive our sessions.
func peerOK(net.Conn) error {
	return errors.New("peer credentials are unavailable on " + runtime.GOOS)
}
