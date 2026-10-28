package testnet

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// recorder is a testing.TB that records how RequireBind ended the test.
type recorder struct {
	testing.TB
	skipped, failed bool
	msg             string
}

// stop unwinds RequireBind as Skip and Fatal would, without ending the
// real test.
type stop struct{}

func (r *recorder) Helper() {}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped, r.msg = true, fmt.Sprintf(format, args...)
	panic(stop{})
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed, r.msg = true, fmt.Sprintf(format, args...)
	panic(stop{})
}

func requireBind(addr string, err error) (r *recorder) {
	r = &recorder{}
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(stop); !ok {
				panic(p)
			}
		}
	}()
	RequireBind(r, addr, err)
	return r
}

// A missing loopback alias skips with that reason; every other failed bind,
// a collision above all, fails; no error carries on.
func TestRequireBind(t *testing.T) {
	wrap := func(errno syscall.Errno) error {
		return &os.SyscallError{Syscall: "bind", Err: errno}
	}
	const block = "127.0.9.10:2152"
	tests := []struct {
		name          string
		addr          string
		err           error
		skip, fail    bool
		wantInMessage string
	}{
		{name: "bound", addr: block, err: nil},
		{name: "no such loopback alias", addr: block, err: wrap(syscall.EADDRNOTAVAIL), skip: true, wantInMessage: "no such loopback address"},
		{name: "no such loopback alias, bare host", addr: "127.0.9.10", err: wrap(syscall.EADDRNOTAVAIL), skip: true, wantInMessage: "no such loopback address"},
		{name: "no such loopback alias, IPv4-mapped", addr: "[::ffff:127.0.9.10]:2152", err: wrap(syscall.EADDRNOTAVAIL), skip: true, wantInMessage: "no such loopback address"},
		{name: "unavailable outside 127/8 (a wrong address)", addr: "192.0.2.10:2152", err: wrap(syscall.EADDRNOTAVAIL), fail: true, wantInMessage: "192.0.2.10:2152"},
		{name: "unavailable on IPv6 loopback", addr: "[::1]:2152", err: wrap(syscall.EADDRNOTAVAIL), fail: true, wantInMessage: "[::1]:2152"},
		{name: "unavailable on an unparsable host", addr: "gnb.invalid:2152", err: wrap(syscall.EADDRNOTAVAIL), fail: true, wantInMessage: "gnb.invalid"},
		{name: "address in use", addr: block, err: wrap(syscall.EADDRINUSE), fail: true, wantInMessage: block},
		{name: "permission denied", addr: block, err: wrap(syscall.EACCES), fail: true, wantInMessage: "permission denied"},
		{name: "not a syscall error", addr: block, err: errors.New("resolve: bad address"), fail: true, wantInMessage: "bad address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := requireBind(tc.addr, tc.err)
			if r.skipped != tc.skip || r.failed != tc.fail {
				t.Fatalf("skipped %t failed %t, want skipped %t failed %t (%q)", r.skipped, r.failed, tc.skip, tc.fail, r.msg)
			}
			if tc.wantInMessage != "" && !strings.Contains(r.msg, tc.wantInMessage) {
				t.Errorf("message %q lacks %q", r.msg, tc.wantInMessage)
			}
		})
	}
}
