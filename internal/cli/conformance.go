package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
)

// newConformanceCmd runs the conformance / regression suite against a live
// core and reports structured results: on the API server by default, as a
// plan streamed over RunConformanceRequest.plan (ADR-0008 §6), or in this
// process with --local (deprecated). It exits 1 if a check fails outside
// the expected list or the harness errors: an integration-CI gate.
func newConformanceCmd(serverURL *string, o options) *cobra.Command {
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
		tf            thinFlags
		gates         []string
		list          bool
	)
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run the core conformance / regression suite against a live core",
		Long: "Run the conformance / regression suite against a live core. It runs on the orbit\n" +
			"server (`orbit serve`) and streams each check's verdict, exiting with the result: 0\n" +
			"passed, 1 a check failed outside the expected list or errored, 2 did not complete, 3\n" +
			"could not start. --detach starts it and returns. --local (deprecated) runs it in this\n" +
			"process. The core, keys and addresses come from the flags or a target (-t).",
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
			if !tf.local {
				return thinConformance(cmd, serverURL, o, gates, tf.detach, jsonOut, expectedPath)
			}
			if tf.detach || len(gates) > 0 {
				return exitWith(ExitCouldNotStart, errors.New("--detach and --gate run on the server; drop --local"))
			}
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), localDeprecated)
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
			ueSpec, err := ueFlags.spec(id.DNN, id.MCC, id.MNC, func(n string) bool { return userGave(cmd.Flags(), n) })
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
				return gateFailed("conformance gate failed: %d fail (%d expected), %d error", sum.Fail, sum.ExpectedFail, sum.Error)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 endpoint (host:port); required unless --list")
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
	tf.add(f, "run the suite in this process, not on the server (deprecated: goes after this release)")
	f.StringArrayVar(&gates, "gate", nil, "add a gate: conformance (the default one), or name=conformance; repeat for more")
	f.Var(newNilStringSlice(&ueFlags.nasIntegrity), "nas-integrity", "NAS integrity algorithms the UE advertises: NIA1, NIA2, NIA3, comma-separated (default NIA2; NIA0 is refused)")
	f.Var(newNilStringSlice(&ueFlags.nasCiphering), "nas-ciphering", "NAS ciphering algorithms the UE advertises: NEA0, NEA1, NEA2, NEA3, comma-separated (default NEA0; another cipher makes the NAS in pcaps unreadable)")
	return cmd
}

// thinConformance runs the suite the flags describe on the server: the
// flags' plan (thinPlan), one conformance workload with the conformance
// gate, streamed over RunConformanceRequest.plan and printed as the local
// run prints it. The run's report then gives the exit. --n3-bind is the
// GTP-U checks' source (conformance.n3_bind), as the request's n3_bind
// was, not the UE's gNB N3 (ran.n3_ips).
func thinConformance(cmd *cobra.Command, serverURL *string, o options, gates []string, detach, jsonOut bool, expectedPath string) error {
	// The list is checked as --local checks it, before anything is built
	// or sent: a bad file is the same error, exit 3.
	if expectedPath != "" {
		if _, err := conformance.LoadExpectedFailures(expectedPath); err != nil {
			return exitWith(ExitCouldNotStart, fmt.Errorf("--expected-failures: %w", err))
		}
	}
	src := target.CommandDefaults(cmd.CommandPath())
	base := []target.Setting{
		{Path: "workloads[conformance].name", Value: "conformance", Source: src},
		{Path: "gates[conformance].conformance", Value: "true", Source: src},
	}
	var extra []target.Setting
	for _, g := range gates {
		s, err := plan.ParseGate(g)
		if err != nil {
			return exitWith(ExitCouldNotStart, err)
		}
		extra = append(extra, s...)
	}
	if f := cmd.Flags().Lookup("n3-bind"); f.Value.String() != "" {
		extra = append(extra, target.Setting{Path: "workloads[conformance].conformance.n3_bind", Value: f.Value.String(),
			Source: target.Source{Name: "--n3-bind"}})
	}
	req, err := thinPlan(cmd, o, base, extra, "n3-bind")
	if err != nil {
		return exitWith(ExitCouldNotStart, err)
	}
	if err := applyServerURL(cmd, serverURL); err != nil {
		return err
	}
	if detach {
		return thinRun(cmd, *serverURL, req, true, waitOpts{})
	}
	p := req.GetPlan()
	stream, err := conformanceClient(*serverURL).RunConformance(cmd.Context(),
		connect.NewRequest(&orbitv1.RunConformanceRequest{Plan: p}))
	if err != nil {
		return noServer(exitWith(ExitCouldNotStart, err), *serverURL)
	}
	var (
		results []conformance.Result
		last    *orbitv1.ConformanceUpdate
	)
	for stream.Receive() {
		u := stream.Msg()
		if r := u.GetResult(); r != nil {
			results = append(results, resultFromProto(r))
		}
		last = u
	}
	if err := stream.Err(); err != nil {
		if last.GetRunId() == "" {
			return noServer(exitWith(ExitCouldNotStart, err), *serverURL)
		}
		return exitWith(ExitDidNotComplete, fmt.Errorf("run %s: %w", last.GetRunId(), err))
	}
	if !last.GetDone() {
		return exitWith(ExitDidNotComplete, fmt.Errorf("run %s: the stream ended before the suite did", last.GetRunId()))
	}
	sum := conformance.Summarize(results)
	sum.TargetRelease = conformance.Release(last.GetTargetRelease())
	for name, rel := range last.GetSpecTargetReleases() {
		sum.SpecTargets[name] = conformance.Release(rel)
	}
	sum.ExpectedFailures = last.GetExpectedFailuresCore()
	for _, st := range last.GetStale() {
		sum.Stale = append(sum.Stale, conformance.StaleExpectation{Check: st.GetCheck(), Finding: st.GetFinding(),
			ExpectedObserved: st.GetExpectedObserved(), Verdict: conformance.Verdict(st.GetVerdict()), Observed: st.GetObserved()})
	}
	core := p.GetTarget().GetCore()
	sum.Network = &conformance.NetworkIdentity{MCC: core.GetPlmn().GetMcc(), MNC: core.GetPlmn().GetMnc(), TAC: core.GetTac()}
	if sl := core.GetSlices(); len(sl) > 0 {
		sum.Network.SST, sum.Network.SD = sl[0].GetSst(), sl[0].GetSd()
	}
	out, judged := cmd.OutOrStdout(), cmd.OutOrStdout()
	if jsonOut {
		b, _ := json.MarshalIndent(sum, "", "  ")
		_, _ = fmt.Fprintln(out, string(b))
		judged = cmd.ErrOrStderr() // stdout is the JSON alone
	} else {
		printConformanceText(out, planNetwork(p.GetTarget()), sum, expectedPath)
	}
	// The run's gates judged it: its report gives the exit, as --wait's.
	runs := *serverURL
	return waitRunWith(cmd.Context(), judged, cmd.ErrOrStderr(), runsClient(&runs), last.GetRunId(), nil, waitOpts{})
}

// resultFromProto is a streamed check result as the suite reports it.
func resultFromProto(r *orbitv1.ConformanceResult) conformance.Result {
	out := conformance.Result{ID: r.GetId(), Category: conformance.Category(r.GetCategory()), SpecRef: r.GetSpecRef(),
		MinRelease: conformance.Release(r.GetMinRelease()), Verdict: conformance.Verdict(r.GetVerdict()),
		Expected: r.GetExpected(), Observed: r.GetObserved(), Detail: r.GetDetail()}
	if x := r.GetExpectedFailure(); x != nil {
		out.ExpectedFailure = &conformance.ExpectedFailure{Finding: x.GetFinding(), Added: x.GetAdded(), Reason: x.GetReason()}
	}
	return out
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
	// nasIntegrity and nasCiphering are --nas-integrity and
	// --nas-ciphering; nil is the default (NIA2, NEA0).
	nasIntegrity, nasCiphering []string
}

// spec builds the suite's UE section. With no --supi there is no UE (the
// mobility checks SKIP), and the other UE flags are refused rather than
// silently ignored when the user gave them (given); values a target
// supplied are the environment's and are simply unused. With one, the keys come from --ki/--opc or
// $ORBIT_KI/$ORBIT_OPC, the SUPI must be in the PLMN and --gnb-n3 must be
// an IPv4 address (canonicalised; see gnb.CanonicalN3).
func (u conformanceUEFlags) spec(dnn, mcc, mnc string, given func(flag string) bool) (conformance.UESpec, error) {
	if u.supi == "" {
		// The NAS algorithm lists are nil unless their flags were given.
		if (u.gnbN3 != "" && given("gnb-n3")) || (u.traffic != "" && given("traffic")) ||
			(u.ki != "" && given("ki")) || (u.opc != "" && given("opc")) ||
			u.nasIntegrity != nil || u.nasCiphering != nil {
			return conformance.UESpec{}, fmt.Errorf("--gnb-n3, --traffic, --ki, --opc, --nas-integrity and --nas-ciphering describe the mobility checks' UE and need --supi")
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
	ia, err := nasAlgorithms("--nas-integrity", "NIA", u.nasIntegrity, 1)
	if err != nil {
		return conformance.UESpec{}, err
	}
	ea, err := nasAlgorithms("--nas-ciphering", "NEA", u.nasCiphering, 0)
	if err != nil {
		return conformance.UESpec{}, err
	}
	return conformance.UESpec{SUPI: u.supi, Ki: ki, OPc: opc, DNN: dnn, GNBN3: n3, Traffic: u.traffic,
		NASIntegrity: ia, NASCiphering: ea}, nil
}

// nasAlgorithms turns a list of algorithm names (prefix followed by a
// digit, case-insensitive) into the UE security capability bitmap, bit 8
// for algorithm 0 (TS 24.501 9.11.3.54). Algorithms 0 to 3 exist; min is
// the lowest one the flag accepts (1 for integrity: NIA0 is refused, since
// the UE never registers for emergency). nil is the default, 0.
func nasAlgorithms(flag, prefix string, names []string, min int) (uint8, error) {
	if names == nil {
		return 0, nil
	}
	if len(names) == 0 {
		return 0, fmt.Errorf("%s: name at least one algorithm", flag)
	}
	var bits uint8
	for _, n := range names {
		up := strings.ToUpper(strings.TrimSpace(n))
		if len(up) != len(prefix)+1 || !strings.HasPrefix(up, prefix) || up[len(prefix)] < '0' || up[len(prefix)] > '3' {
			return 0, fmt.Errorf("%s: %q is not one of %s%d to %s3", flag, n, prefix, min, prefix)
		}
		a := int(up[len(prefix)] - '0')
		if a < min {
			return 0, fmt.Errorf("%s: %s is refused: the UE never registers for emergency, so it does not offer NIA0", flag, up)
		}
		bits |= 0x80 >> a
	}
	return bits, nil
}

// nilStringSlice is a comma-separated, repeatable string list flag that
// stays nil until given, so "not set" differs from an empty list.
type nilStringSlice struct{ v *[]string }

func newNilStringSlice(v *[]string) *nilStringSlice { return &nilStringSlice{v: v} }

func (s *nilStringSlice) String() string { return strings.Join(*s.v, ",") }

func (s *nilStringSlice) Set(val string) error {
	if *s.v == nil {
		*s.v = []string{}
	}
	for _, p := range strings.Split(val, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			return fmt.Errorf("empty element in %q", val)
		}
		*s.v = append(*s.v, p)
	}
	return nil
}

func (s *nilStringSlice) Type() string { return "strings" }
