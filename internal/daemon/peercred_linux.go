//go:build linux

package daemon

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerOK reports whether the process on the other end of conn runs as our user, so
// another account cannot drive our sessions. Every instance here is the same user.
func peerOK(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil // an in-process pipe, from a test; a daemon only ever listens on a socket
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var err2 error
	if err := raw.Control(func(fd uintptr) {
		cred, err2 = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}
	if uid := int(cred.Uid); uid != os.Getuid() {
		return fmt.Errorf("peer runs as uid %d, not %d", uid, os.Getuid())
	}
	return nil
}
