package sctp

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	isctp "github.com/ishidawataru/sctp"
	"golang.org/x/sys/unix"
)

// subscribedEvents reads the SCTP_EVENTS option of fd into a heap buffer,
// as long as the kernel's struct sctp_event_subscribe.
func subscribedEvents(t testing.TB, fd int) []byte {
	t.Helper()
	buf := make([]byte, 64)
	n := uint32(len(buf))
	if _, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), isctp.SOL_SCTP, isctp.SCTP_EVENTS,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0); errno != 0 {
		t.Fatalf("getsockopt SCTP_EVENTS: %v", errno)
	}
	return buf[:n]
}

// sctpSocket opens an unconnected SCTP socket, or skips without kernel SCTP.
func sctpSocket(t testing.TB) *isctp.SCTPConn {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_SCTP)
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	return isctp.NewSCTPConn(fd, nil)
}

// These tests hold the pinned SCTP library to two things ORBIT relies on:
// SubscribeEvents sets exactly the events asked for, and the socket option
// reads behind LocalAddr and Addr are correct, both when the goroutine's
// stack moves around the call.

// SubscribeEvents sets exactly the events asked for, each in its field of
// struct sctp_event_subscribe, and clears the others.
func TestSubscribeEvents(t *testing.T) {
	tests := []struct {
		name  string
		flags int
		want  []byte
	}{
		{name: "data I/O", flags: isctp.SCTP_EVENT_DATA_IO, want: []byte{1}},
		{name: "none", flags: 0, want: nil},
		{name: "data I/O and shutdown", flags: isctp.SCTP_EVENT_DATA_IO | isctp.SCTP_EVENT_SHUTDOWN, want: []byte{1, 0, 0, 0, 0, 1}},
		{name: "sender dry", flags: isctp.SCTP_EVENT_SENDER_DRY, want: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := sctpSocket(t)
			defer func() { _ = c.Close() }()
			// Start from every event on, so a field left alone shows.
			if err := c.SubscribeEvents(isctp.SCTP_EVENT_ALL); err != nil {
				t.Fatal(err)
			}
			if err := c.SubscribeEvents(tc.flags); err != nil {
				t.Fatal(err)
			}
			rc, err := c.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			if err := rc.Control(func(fd uintptr) { got = subscribedEvents(t, int(fd)) }); err != nil {
				t.Fatal(err)
			}
			want := make([]byte, len(got))
			copy(want, tc.want)
			if !bytes.Equal(got, want) {
				t.Errorf("SCTP_EVENTS = %x, want %x", got, want)
			}
		})
	}
}

// A closed connection is an error, not a subscription of some other socket.
func TestSubscribeEventsClosed(t *testing.T) {
	c := sctpSocket(t)
	_ = c.Close()
	if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err == nil {
		t.Error("SubscribeEvents on a closed connection succeeded")
	}
}

// deepen calls f n frames down, so f runs at a stack depth that varies
// with n and the goroutine's stack grows at a different point each time.
//
//go:noinline
func deepen(n int, f func()) {
	var pad [8]byte
	if n > 0 {
		deepen(n-1, f)
		_ = pad
		return
	}
	f()
}

// Run from fresh goroutines at varying stack depths, so stacks grow and
// move around the call, every subscription is exactly the one asked for.
func TestSubscribeEventsAcrossStackMoves(t *testing.T) {
	const n = 20000
	_ = sctpSocket(t).Close() // skips without kernel SCTP
	var mu sync.Mutex
	var wrong []string
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go deepen(i%3000, func() {
			defer wg.Done()
			fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_SCTP)
			if err != nil {
				mu.Lock()
				wrong = append(wrong, err.Error())
				mu.Unlock()
				return
			}
			c := isctp.NewSCTPConn(fd, nil)
			defer func() { _ = c.Close() }()
			if err := c.SubscribeEvents(isctp.SCTP_EVENT_DATA_IO); err != nil {
				mu.Lock()
				wrong = append(wrong, err.Error())
				mu.Unlock()
				return
			}
			if got := subscribedEvents(t, fd); len(got) == 0 || got[0] != 1 || bytes.Count(got[1:], []byte{0}) != len(got)-1 {
				mu.Lock()
				wrong = append(wrong, string(got))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(wrong) != 0 {
		t.Errorf("%d of %d subscriptions wrong, first %q", len(wrong), n, wrong[0])
	}
}

// Run from fresh goroutines at varying stack depths, so stacks grow and
// move around the call, every address read returns the socket's address.
func TestAddrAcrossStackMoves(t *testing.T) {
	const n = 20000
	_ = sctpSocket(t).Close() // skips without kernel SCTP
	loopback := []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}
	ln, err := isctp.ListenSCTP("sctp", &isctp.SCTPAddr{IPAddrs: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	lport := ln.Addr().(*isctp.SCTPAddr).Port
	served := make(chan struct{})
	go func() {
		defer close(served)
		c, err := ln.Accept()
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = c.Read(make([]byte, 1)) // holds the association until the dialer closes
		}
	}()
	// Runs after the dialer's and before the listener's deferred Close, so
	// the server side has ended when the test returns.
	defer func() { _ = ln.Close(); <-served }()
	c, err := isctp.DialSCTP("sctp", nil, &isctp.SCTPAddr{IPAddrs: loopback, Port: lport})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	cport := c.LocalAddr().(*isctp.SCTPAddr).Port
	var mu sync.Mutex
	var wrong []string
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go deepen(i%3000, func() {
			defer wg.Done()
			la, lok := ln.Addr().(*isctp.SCTPAddr)
			ca, cok := c.LocalAddr().(*isctp.SCTPAddr)
			if !lok || la.Port != lport || !cok || ca.Port != cport {
				mu.Lock()
				wrong = append(wrong, fmt.Sprintf("listener %v, conn %v", la, ca))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(wrong) != 0 {
		t.Errorf("%d of %d reads wrong (want listener port %d, conn port %d), first %s", len(wrong), n, lport, cport, wrong[0])
	}
}
