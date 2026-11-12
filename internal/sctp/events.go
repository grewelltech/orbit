package sctp

import (
	"fmt"
	"syscall"

	isctp "github.com/ishidawataru/sctp"
	"golang.org/x/sys/unix"
)

// eventFlags are the library's SCTP_EVENT_* flags in the order of the
// fields of struct sctp_event_subscribe (RFC 6458 6.2.1, as Linux lays it
// out) that they set.
var eventFlags = []int{
	isctp.SCTP_EVENT_DATA_IO,
	isctp.SCTP_EVENT_ASSOCIATION,
	isctp.SCTP_EVENT_ADDRESS,
	isctp.SCTP_EVENT_SEND_FAILURE,
	isctp.SCTP_EVENT_PEER_ERROR,
	isctp.SCTP_EVENT_SHUTDOWN,
	isctp.SCTP_EVENT_PARTIAL_DELIVERY,
	isctp.SCTP_EVENT_ADAPTATION_LAYER,
	isctp.SCTP_EVENT_AUTHENTICATION,
	isctp.SCTP_EVENT_SENDER_DRY,
}

// eventSubscription is the SCTP_EVENTS option value that subscribes to the
// events flags names (the library's SCTP_EVENT_* bits) and to no other.
func eventSubscription(flags int) []byte {
	b := make([]byte, len(eventFlags))
	for i, f := range eventFlags {
		if flags&f != 0 {
			b[i] = 1
		}
	}
	return b
}

// SubscribeEvents sets the SCTP_EVENTS option of the SCTP socket behind c
// to the events flags names, as isctp.SCTPConn.SubscribeEvents does. The
// option value is handed to the kernel from the heap through
// unix.SetsockoptString. The library's method passes the address of a
// stack variable to the kernel as a uintptr through an ordinary function,
// which the garbage collector does not track: when the goroutine's stack
// moves before the system call, the kernel reads stale memory and the
// socket is left with other subscriptions than the ones asked for. Without
// SCTP_EVENT_DATA_IO a read reports no stream or PPID, and with an event
// subscribed that nobody asked for a notification is read as a message.
func SubscribeEvents(c syscall.Conn, flags int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return fmt.Errorf("sctp: SCTP_EVENTS: %w", err)
	}
	opt := string(eventSubscription(flags))
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptString(int(fd), isctp.SOL_SCTP, isctp.SCTP_EVENTS, opt)
	}); err != nil {
		return fmt.Errorf("sctp: SCTP_EVENTS: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("sctp: SCTP_EVENTS: %w", serr)
	}
	return nil
}

// kernelConn is the association kernelDial returns: the library's
// connection with SubscribeEvents replaced by the package's.
type kernelConn struct{ *isctp.SCTPConn }

// SubscribeEvents subscribes the association to the events flags names
// (see the package function SubscribeEvents).
func (c kernelConn) SubscribeEvents(flags int) error { return SubscribeEvents(c.SCTPConn, flags) }
