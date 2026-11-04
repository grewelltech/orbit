package sctp_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Many quick dial-and-close cycles from ephemeral ports against one loopback
// listener: the pattern in which a fresh socket draws the port of an
// association still closing and the connect collides with it (EISCONN,
// EALREADY, or the peer's ECONNREFUSED; about 1 in 2,400 dials before the
// retry). With the retry no dial fails on a collision. A dial whose INIT the
// peer silently discards still times out after its bound; that residual is
// logged, not failed, since the retry cannot see it. Opt-in (ORBIT_SCTP_STRESS=1:
// it takes about 20 s, more when a dial times out) and skipped without kernel
// SCTP.
func TestDialStressEphemeralPorts(t *testing.T) {
	if os.Getenv("ORBIT_SCTP_STRESS") != "1" {
		t.Skip("set ORBIT_SCTP_STRESS=1 to run the ephemeral-port dial stress test")
	}
	const workers, dials = 8, 300
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		failures  int
		collision int
		first     error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < dials; i++ {
				c, err := sctp.Dial(context.Background(), "127.0.0.1", front.Addr)
				if err != nil {
					mu.Lock()
					failures++
					if errors.Is(err, syscall.EISCONN) || errors.Is(err, syscall.EALREADY) || errors.Is(err, syscall.ECONNREFUSED) {
						collision++
					}
					if first == nil {
						first = err
					}
					mu.Unlock()
					continue
				}
				_ = c.Close()
			}
		}()
	}
	wg.Wait()
	if collision > 0 {
		t.Errorf("%d of %d dials failed on a collision; first failure: %v", collision, workers*dials, first)
	}
	if other := failures - collision; other > 0 {
		t.Logf("%d of %d dials failed otherwise (the discarded-INIT residual); first failure: %v", other, workers*dials, first)
	}
}
