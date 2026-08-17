package systemd

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// notifyTimeout bounds the sd_notify datagram round-trip. sd_notify(3)
// itself has no timeout, but a hung socket must not stall lifecycle
// paths, so both the dial and the write are bounded.
const notifyTimeout = 3 * time.Second

// SdNotify sends a state-change notification (e.g. "READY=1") to the
// service manager over the socket named in NOTIFY_SOCKET, following the
// sd_notify(3) wire protocol: one message, newline-terminated, on a
// datagram socket. It is a no-op when NOTIFY_SOCKET is unset, so the
// daemon also works when started outside systemd.
func SdNotify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	conn, err := net.DialTimeout("unixgram", notifyAddress(sock), notifyTimeout)
	if err != nil {
		return fmt.Errorf("sd_notify: dial %q: %w", sock, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(notifyTimeout)); err != nil {
		return fmt.Errorf("sd_notify: set deadline: %w", err)
	}
	if _, err := conn.Write([]byte(state + "\n")); err != nil {
		return fmt.Errorf("sd_notify: write: %w", err)
	}
	return nil
}

// notifyAddress maps a NOTIFY_SOCKET value to a socket address. A
// leading "@" denotes a Linux abstract socket and is replaced with a
// leading NUL byte; Go passes that name to the kernel verbatim, which
// Linux interprets as abstract. Everything else is a filesystem path.
func notifyAddress(sock string) string {
	if strings.HasPrefix(sock, "@") {
		return "\x00" + sock[1:]
	}
	return sock
}
