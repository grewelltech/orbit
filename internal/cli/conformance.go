package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/ue"
)

// newConformanceCmd runs the conformance / regression suite in-process against
// a live core and reports structured results. It exits non-zero if any check
// fails or the harness errors — an integration-CI gate.
func newConformanceCmd() *cobra.Command {
	var (
		amf           string
		bind          string
		upfN3, n3Bind string
		gnbBase       uint32
		categories    []string
		jsonOut       bool
		perTest       time.Duration
		id            netIdentity
		targetRelease string
		expectedPath  string
		ueFlags       conformanceUEFlags
		list          bool
	)
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run the core conformance / regression suite against a live core",
		RunE: func(cmd *cobra.Command, args []string) error {
			cats := make([]conformance.Category, 0, len(categories))
			for _, c := range categories {
				cats = append(cats, conformance.Category(c))
			}
			reg := conformance.NewRegistry()
			if list {
				return printConformanceList(cmd.OutOrStdout(), reg.Select(cats...), perTest)
			}
			if amf == "" {
				return fmt.Errorf(`required flag(s) "amf" not set`)
			}
			// The list is read first, so a bad file fails before any dial.
			var expected *conformance.ExpectedFailures
			if expectedPath != "" {
				x, err := conformance.LoadExpectedFailures(expectedPath)
				if err != nil {
					return fmt.Errorf("--expected-failures: %w", err)
				}
				expected = x
			}
			if err := conformance.ValidateBind(bind); err != nil {
				return fmt.Errorf("--bind: %w", err)
			}
			if err := id.resolve(true); err != nil {
				return err
			}
			ueSpec, err := ueFlags.spec(id.DNN, id.MCC, id.MNC)
			if err != nil {
				return err
			}
			target := conformance.DefaultTargetRelease
			if targetRelease != "" {
				t, err := conformance.ParseRelease(targetRelease)
				if err != nil {
					return fmt.Errorf("--target-release: %w", err)
				}
				target = t
			}
			env := conformance.Env{
				AMFAddr:       amf,
				Bind:          bind,
				UPFN3:         upfN3,
				N3Bind:        n3Bind,
				GNB:           id.gnbConfig(gnbBase, 0, "orbit-conf"),
				TargetRelease: target,
				SpecTargets:   conformance.DefaultSpecTargets(),
				UE:            ueSpec,
			}
			// Each check is announced on stderr before it runs, with how long
			// it may take, so a check that waits out NAS timers does not look
			// like a hang; stdout keeps only the results.
			ann := newAnnouncer(cmd.ErrOrStderr(), progressEvery)
			ann.suite(reg.Select(cats...), perTest)
			var results []conformance.Result
			reg.RunWithStart(cmd.Context(), env, perTest, ann.start, func(res conformance.Result, _, _ int) {
				ann.done()
				results = append(results, res)
			}, cats...)
			expected.Apply(results)
			sum := conformance.Summarize(results)
			sum.Stale = expected.Stale(results)
			sum.ExpectedFailures = expected.Label()
			sum.N2Source = env.N2Source()
			sum.Network = env.Network()
			sum.TargetRelease = target
			sum.SpecTargets = env.NarrowingSpecTargets()

			out := cmd.OutOrStdout()
			if jsonOut {
				b, _ := json.MarshalIndent(sum, "", "  ")
				fmt.Fprintln(out, string(b))
			} else {
				printConformanceText(out, id.String(), sum, expectedPath)
			}
			if !sum.OK() {
				return fmt.Errorf("conformance gate failed: %d fail (%d expected), %d error", sum.Fail, sum.ExpectedFail, sum.Error)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 endpoint (host:port)")
	f.StringVar(&bind, "bind", "", bindFlagHelp+"; port must be 0 (each check dials afresh)")
	addNetIdentityFlags(f, &id, withDNN())
	f.StringVar(&upfN3, "upf-n3", "", "UPF N3 endpoint host:port: the UPF's primary N3 address, which answers GTP-U Echo (enables GTP-U checks; run from the UPF access network)")
	f.StringVar(&n3Bind, "n3-bind", "", "local N3 source host:port for GTP-U checks (e.g. 172.17.50.12:2152)")
	f.Uint32Var(&gnbBase, "gnb-base", 0x400, "base gNB ID (each check uses a distinct one)")
	f.StringSliceVar(&categories, "category", nil, "only run these categories (procedural, negative-ie, security, gtpu, timing, mobility, nas)")
	f.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	f.DurationVar(&perTest, "per-test-timeout", conformance.DefaultPerTestTimeout, "per-check timeout; a check that needs longer (NAS timer checks) gets its own budget")
	f.BoolVar(&list, "list", false, "list the checks (filtered by --category) with how long each can take at --per-test-timeout, and the total, without running them")
	f.StringVar(&targetRelease, "target-release", "",
		"3GPP release the core claims (Rel-15 or 15); checks that require a newer release SKIP, never FAIL (default Rel-18, the SD-Core release; TS 29.244 pinned to Rel-16; \"unset\" runs every check)")
	f.StringVar(&expectedPath, "expected-failures", "",
		"YAML list of this core's known, tolerated FAILs (docs/conformance/expected-failures.md); a listed FAIL still prints FAIL but does not fail the gate")
	f.StringVar(&ueFlags.supi, "supi", "", supiFlagHelp+"; enables the mobility checks")
	f.StringVar(&ueFlags.ki, "ki", "", kiFlagHelp)
	f.StringVar(&ueFlags.opc, "opc", "", opcFlagHelp)
	f.StringVar(&ueFlags.gnbN3, "gnb-n3", "", "gNB N3 IP for the mobility checks' data path (reachable from the UPF; both gNBs use it)")
	f.StringVar(&ueFlags.traffic, "traffic", "", "optional echo peer host:port the mobility checks' UE sends to across the handover")
	return cmd
}

// printConformanceList prints --list: one line per check with its category,
// required release and the longest it can take at perTest, then the total.
func printConformanceList(w io.Writer, tests []conformance.Test, perTest time.Duration) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHECK\tCATEGORY\tREQUIRES\tUP TO")
	var total time.Duration
	for _, t := range tests {
		d := conformance.ExpectedDuration(t, perTest)
		total += d
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.ID(), t.Category(), conformance.MinRelease(t), d)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%d checks, up to %s in total at --per-test-timeout %s\n", len(tests), total, perTestOrDefault(perTest))
	return err
}

func perTestOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return conformance.DefaultPerTestTimeout
	}
	return d
}

// progressEvery is how often a long check reports that it is still running.
const progressEvery = 30 * time.Second

// announcer prints the run's progress to w (stderr): the suite's total
// before it starts, each check as it starts with the longest it can take,
// and, for a check that can take longer than every, its elapsed time every
// every. It is driven from the suite's goroutine (start, done) plus one
// ticker goroutine per long check.
type announcer struct {
	w     io.Writer
	every time.Duration

	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

func newAnnouncer(w io.Writer, every time.Duration) *announcer {
	return &announcer{w: w, every: every}
}

// suite prints how many checks will run and the longest the run can take.
func (a *announcer) suite(tests []conformance.Test, perTest time.Duration) {
	var total time.Duration
	for _, t := range tests {
		total += conformance.ExpectedDuration(t, perTest)
	}
	a.printf("Running %d checks, up to %s in total\n", len(tests), total)
}

// start announces a check; a long one gets a progress line every a.every.
func (a *announcer) start(t conformance.Test, expected time.Duration, index, total int) {
	a.printf("[%d/%d] %s (%s): up to %s\n", index+1, total, t.ID(), t.Category(), expected)
	if expected <= a.every {
		return
	}
	stop := make(chan struct{})
	a.mu.Lock()
	a.stop = stop
	a.mu.Unlock()
	began := time.Now()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		tick := time.NewTicker(a.every)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				a.printf("      %s: %s elapsed of up to %s\n", t.ID(), time.Since(began).Round(time.Second), expected)
			}
		}
	}()
}

// done ends the running check's progress lines.
func (a *announcer) done() {
	a.mu.Lock()
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	a.wg.Wait()
}

func (a *announcer) printf(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = fmt.Fprintf(a.w, format, args...)
}

// printConformanceText renders a run for the terminal: the network, source,
// targets and expected-failures list it used, one line per check (a FAIL the
// list tolerates says so) with its detail, any stale entries, and the tally.
func printConformanceText(out io.Writer, network string, sum conformance.Summary, expectedPath string) {
	_, _ = fmt.Fprintf(out, "network: %s\n", network)
	if sum.N2Source != "" {
		_, _ = fmt.Fprintf(out, "n2-source=%s\n", sum.N2Source)
	}
	_, _ = fmt.Fprintf(out, "target-release: %s\n", sum.TargetLine())
	if sum.ExpectedFailures != "" {
		_, _ = fmt.Fprintf(out, "expected-failures: %s (%s)\n", sum.ExpectedFailures, expectedPath)
	}
	for _, r := range sum.Results {
		verdict := string(r.Verdict)
		if r.ExpectedFailure != nil {
			verdict += " (expected: " + r.ExpectedFailure.Finding + ")"
		}
		_, _ = fmt.Fprintf(out, "%-5s [%-11s] %-26s %s\n", verdict, r.Category, r.ID, r.Observed)
		// Detail always ends with the requirement line: the release the
		// check requires and its clauses.
		_, _ = fmt.Fprintf(out, "      %s\n", r.Detail)
	}
	// Stale entries warn so the list gets pruned; they do not gate.
	for _, st := range sum.Stale {
		_, _ = fmt.Fprintf(out, "stale expected failure: %s %s: now %s %q\n", st.Check, st.Finding, st.Verdict, st.Observed)
	}
	_, _ = fmt.Fprintf(out, "\n%d checks: %d pass, %d fail (%d expected), %d deviation, %d error, %d skip\n",
		sum.Total, sum.Pass, sum.Fail, sum.ExpectedFail, sum.Deviation, sum.Error, sum.Skip)
}

// conformanceUEFlags are the conformance flags describing the UE the
// mobility checks register.
type conformanceUEFlags struct {
	supi, ki, opc, gnbN3, traffic string
}

// spec builds the suite's UE section. With no --supi there is no UE (the
// mobility checks SKIP), and the other UE flags are refused rather than
// silently ignored. With one, the keys come from --ki/--opc or
// $ORBIT_KI/$ORBIT_OPC, the SUPI must be in the PLMN and --gnb-n3 must be
// an IPv4 address (canonicalised; see gnb.CanonicalN3).
func (u conformanceUEFlags) spec(dnn, mcc, mnc string) (conformance.UESpec, error) {
	if u.supi == "" {
		if u.gnbN3 != "" || u.traffic != "" || u.ki != "" || u.opc != "" {
			return conformance.UESpec{}, fmt.Errorf("--gnb-n3, --traffic, --ki and --opc describe the mobility checks' UE and need --supi")
		}
		return conformance.UESpec{}, nil
	}
	if _, err := ue.ParseIdentity(u.supi, mcc, mnc, "0"); err != nil {
		return conformance.UESpec{}, fmt.Errorf("--supi: %w", err)
	}
	ki, opc, err := resolveCreds(u.ki, u.opc)
	if err != nil {
		return conformance.UESpec{}, err
	}
	if u.gnbN3 == "" {
		return conformance.UESpec{}, fmt.Errorf("--gnb-n3: the mobility checks need the gNB N3 IP with --supi")
	}
	n3, err := gnbN3Flag(u.gnbN3)
	if err != nil {
		return conformance.UESpec{}, err
	}
	if u.traffic != "" {
		if _, _, err := net.SplitHostPort(u.traffic); err != nil {
			return conformance.UESpec{}, fmt.Errorf("--traffic: want host:port: %w", err)
		}
	}
	return conformance.UESpec{SUPI: u.supi, Ki: ki, OPc: opc, DNN: dnn, GNBN3: n3, Traffic: u.traffic}, nil
}
