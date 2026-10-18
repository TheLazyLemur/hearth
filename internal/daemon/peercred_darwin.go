//go:build darwin

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
	var uid uint32
	var err2 error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if cerr != nil {
			err2 = cerr
			return
		}
		uid = cred.Uid
	}); err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}
	if uid != uint32(os.Getuid()) {
		return fmt.Errorf("peer runs as uid %d, not %d", uid, os.Getuid())
	}
	return nil
}
