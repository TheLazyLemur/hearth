package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// attachTimeout is how long a client waits for the daemon it started to bind its socket.
const attachTimeout = 3 * time.Second

// Attach connects to the daemon for home, starting one when none runs, and returns the
// client. A daemon of another protocol version makes way for ours unless it is running
// turns, which stopping it would cut short.
func Attach(home string) (*Client, error) {
	sock := SocketPath(home)
	c, err := Dial(sock)
	if err == nil {
		return c, nil
	}
	var v *VersionError
	if errors.As(err, &v) && v.Busy {
		return nil, err
	}
	// nothing is listening, or an idle daemon of another build is on its way out
	started := startDaemon(home)
	for deadline := time.Now().Add(attachTimeout); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if c, err = Dial(sock); err == nil {
			return c, nil
		}
	}
	if started != nil {
		return nil, started
	}
	return nil, err
}

// startDaemon runs `hearth daemon` in a session of its own, so it outlives the instance that
// needed it, with its output in the log file. One already running makes this a no-op.
func startDaemon(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(LogPath(home), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Env = append(os.Environ(), "HEARTH_HOME="+home)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
