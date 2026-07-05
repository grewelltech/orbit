package mockamf_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestMuxManyUEsOneAssociation attaches N UEs multiplexed over a SINGLE gNB
// association (one shared pipe, demuxed by RAN-UE-NGAP-ID) with bounded
// concurrency — the real gNB pattern and the D-6-informed actor model. It
// proves the gnb.Session demux and per-UE transports work end to end without
// a core.
func TestMuxManyUEsOneAssociation(t *testing.T) {
	const (
		nUEs    = 60
		workers = 16
	)
	amf := testAMF(t)
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// One shared association for the whole gNB, NG Setup once.
	session, err := gnb.Dial(ctx, amf.ConnectShared(ctx), gnbCfg)
	if err != nil {
		t.Fatalf("gNB session dial: %v", err)
	}
	defer session.Close()

	var registered atomic.Int64
	jobs := make(chan int, nUEs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ { // bounded concurrency (D-6)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				uet, ranID := session.NewUE()
				supi := fmt.Sprintf("%015d", int64(208930100007500)+int64(i))
				id, err := ue.ParseIdentity(supi, "208", "93", "0")
				if err != nil {
					t.Errorf("ue %d: %v", i, err)
					continue
				}
				sess, err := engine.Attach(ctx, uet, gnbCfg, engine.UEConfig{
					Identity:    id,
					Sub:         auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
					RANUENGAPID: ranID,
				}, log, nil)
				uet.Close()
				if err != nil {
					t.Errorf("ue %d (ran %d): attach: %v", i, ranID, err)
					continue
				}
				if sess.Result.Registered {
					registered.Add(1)
				}
			}
		}()
	}
	for i := 0; i < nUEs; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if got := registered.Load(); got != nUEs {
		t.Fatalf("registered %d/%d UEs over one association", got, nUEs)
	}
	t.Logf("%d UEs registered over a single gNB association (bounded %d workers)", nUEs, workers)
}
