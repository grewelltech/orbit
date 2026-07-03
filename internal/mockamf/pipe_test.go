package mockamf

import (
	"bytes"
	"testing"

	"github.com/bgrewell/orbit/internal/gnb"
)

// The pipe endpoints must satisfy gnb.Transport so engine.Attach can drive
// one end while the mock AMF handler drives the other.
var _ gnb.Transport = (*pipe)(nil)

func TestPipeRoundTrip(t *testing.T) {
	ue, amf := newPipePair()
	defer ue.Close()

	msg := []byte{0x00, 0x15, 0x00, 0x39} // arbitrary NGAP-ish bytes
	if err := ue.WriteNGAP(1, msg); err != nil {
		t.Fatal(err)
	}
	got, stream, ppid, err := amf.ReadMsg(make([]byte, 1024))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Errorf("read %x, want %x", got, msg)
	}
	if stream != 1 || ppid != ppidNGAP {
		t.Errorf("stream=%d ppid=%d, want 1/%d", stream, ppid, ppidNGAP)
	}

	// The write is copied, so mutating the caller's buffer is safe.
	msg[0] = 0xFF
	if got[0] == 0xFF {
		t.Error("pipe did not copy the written payload")
	}
}

func TestPipeCloseWakesReader(t *testing.T) {
	ue, amf := newPipePair()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := amf.ReadMsg(make([]byte, 16))
		done <- err
	}()
	ue.Close() // closing either end must wake a blocked reader on the other
	if err := <-done; err == nil {
		t.Error("expected an error after close")
	}
}
