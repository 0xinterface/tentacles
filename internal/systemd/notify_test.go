package systemd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := SdNotify("READY=1"); err != nil {
		t.Fatalf("SdNotify with unset NOTIFY_SOCKET: %v", err)
	}
}

func TestSdNotifyDeliversDatagram(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "notify.sock")
	// Unix socket paths are length-limited on every platform; skip when
	// the temp dir pushes us past the limit.
	if len(sock) >= 104 {
		t.Skipf("socket path too long (%d chars): %s", len(sock), sock)
	}

	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer l.Close()
	// Owner-only, like systemd's own notify socket.
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NOTIFY_SOCKET", sock)
	if err := SdNotify("READY=1"); err != nil {
		t.Fatalf("SdNotify: %v", err)
	}

	buf := make([]byte, 256)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1\n" {
		t.Fatalf("received %q, want %q", got, "READY=1\n")
	}
}

func TestSdNotifyMissingSocketErrors(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "does-not-exist.sock")
	t.Setenv("NOTIFY_SOCKET", sock)
	if err := SdNotify("READY=1"); err == nil {
		t.Fatal("expected error dialing missing socket")
	}
}

func TestNotifyAddress(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"@/org/freedesktop/systemd1/notify", "\x00/org/freedesktop/systemd1/notify"},
		{"/run/systemd/notify", "/run/systemd/notify"},
		{"@", "\x00"},
	}
	for _, c := range cases {
		if got := notifyAddress(c.in); got != c.want {
			t.Errorf("notifyAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSdNotifyNewlineTerminated(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "notify2.sock")
	if len(sock) >= 104 {
		t.Skipf("socket path too long (%d chars): %s", len(sock), sock)
	}
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer l.Close()

	t.Setenv("NOTIFY_SOCKET", sock)
	if err := SdNotify("STATUS=done"); err != nil {
		t.Fatalf("SdNotify: %v", err)
	}
	buf := make([]byte, 256)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg := string(buf[:n])
	if !strings.HasSuffix(msg, "\n") {
		t.Fatalf("message %q not newline-terminated", msg)
	}
	if !strings.HasPrefix(msg, "STATUS=done\n") {
		t.Fatalf("message %q, want prefix STATUS=done\n", msg)
	}
}
