package mockamf_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestD6 measures ORBIT's sim-capacity — the UE-actor cost at scale — against
// the in-process mock AMF, to decide goroutine-per-UE vs a bounded worker
// pool (discovery spike D-6). Guarded by ORBIT_D6 so it stays out of normal
// CI. Run each mode in its own process for a clean peak-RSS reading:
//
//	ORBIT_D6=1 ORBIT_D6_MODE=goroutine ORBIT_D6_COUNT=10000 \
//	  go test ./internal/mockamf -run TestD6 -v -timeout 20m
//	ORBIT_D6=1 ORBIT_D6_MODE=pool ORBIT_D6_WORKERS=256 ORBIT_D6_COUNT=10000 \
//	  go test ./internal/mockamf -run TestD6 -v -timeout 20m
func TestD6(t *testing.T) {
	if os.Getenv("ORBIT_D6") == "" {
		t.Skip("set ORBIT_D6=1 to run the D-6 sim-capacity spike")
	}
	mode := envOr("ORBIT_D6_MODE", "goroutine")
	count := envInt("ORBIT_D6_COUNT", 10000)
	workers := envInt("ORBIT_D6_WORKERS", 256)

	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	})
	if err != nil {
		t.Fatal(err)
	}
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	attach := func(i int) (time.Duration, error) {
		supi := fmt.Sprintf("%015d", int64(208930100000000)+int64(i))
		id, err := ue.ParseIdentity(supi, "208", "93", "0")
		if err != nil {
			return 0, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		conn := amf.Connect(ctx)
		defer conn.Close()
		if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
			return 0, fmt.Errorf("ng setup: %v", err)
		}
		sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
			Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		}, log, nil)
		if err != nil {
			return 0, err
		}
		if !sess.Result.Registered {
			return 0, fmt.Errorf("not registered")
		}
		return time.Since(start), nil
	}

	lat := make([]time.Duration, count)
	var failures int64
	var peakGoroutines int64
	stop := make(chan struct{})
	go func() { // sample peak goroutines during the run
		for {
			select {
			case <-stop:
				return
			default:
				if g := int64(runtime.NumGoroutine()); g > peakGoroutines {
					peakGoroutines = g
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	wall := time.Now()
	switch mode {
	case "goroutine":
		var wg sync.WaitGroup
		wg.Add(count)
		for i := 0; i < count; i++ {
			go func(i int) {
				defer wg.Done()
				d, err := attach(i)
				if err != nil {
					failures++
					return
				}
				lat[i] = d
			}(i)
		}
		wg.Wait()
	case "pool":
		jobs := make(chan int, count)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					d, err := attach(i)
					if err != nil {
						failures++
						continue
					}
					lat[i] = d
				}
			}()
		}
		for i := 0; i < count; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	wallDur := time.Since(wall)
	close(stop)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	report(t, mode, count, workers, lat, failures, wallDur, peakGoroutines, &ms)
}

func report(t *testing.T, mode string, count, workers int, lat []time.Duration, failures int64, wall time.Duration, peakG int64, ms *runtime.MemStats) {
	ok := lat[:0]
	for _, d := range lat {
		if d > 0 {
			ok = append(ok, d)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
	pct := func(p float64) time.Duration {
		if len(ok) == 0 {
			return 0
		}
		idx := int(p / 100 * float64(len(ok)))
		if idx >= len(ok) {
			idx = len(ok) - 1
		}
		return ok[idx]
	}
	rate := float64(count-int(failures)) / wall.Seconds()
	t.Logf("D-6 %s: count=%d workers=%d failures=%d wall=%s rate=%.0f/s",
		mode, count, workers, failures, wall.Round(time.Millisecond), rate)
	t.Logf("  latency p50=%s p90=%s p99=%s p99.9=%s max=%s",
		pct(50).Round(time.Microsecond), pct(90).Round(time.Microsecond),
		pct(99).Round(time.Microsecond), pct(99.9).Round(time.Microsecond), pct(100).Round(time.Microsecond))
	t.Logf("  peak goroutines=%d  peak RSS(VmHWM)=%s  Go Sys=%dMB HeapInuse=%dMB",
		peakG, vmHWM(), ms.Sys>>20, ms.HeapInuse>>20)
}

// vmHWM reads the process peak resident set size from /proc/self/status.
func vmHWM() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "n/a"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "n/a"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
