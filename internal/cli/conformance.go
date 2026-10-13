package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	)
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run the core conformance / regression suite against a live core",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			cats := make([]conformance.Category, 0, len(categories))
			for _, c := range categories {
				cats = append(cats, conformance.Category(c))
			}
			results := conformance.NewRegistry().Run(cmd.Context(), env, perTest, cats...)
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
	f.StringVar(&upfN3, "upf-n3", "", "UPF N3 endpoint host:port (enables GTP-U checks; run from the UPF access network)")
	f.StringVar(&n3Bind, "n3-bind", "", "local N3 source host:port for GTP-U checks (e.g. 172.17.50.12:2152)")
	f.Uint32Var(&gnbBase, "gnb-base", 0x400, "base gNB ID (each check uses a distinct one)")
	f.StringSliceVar(&categories, "category", nil, "only run these categories (procedural, negative-ie, security, gtpu, timing, mobility)")
	f.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	f.DurationVar(&perTest, "per-test-timeout", 15*time.Second, "per-check timeout")
	f.StringVar(&targetRelease, "target-release", "",
		"3GPP release the core claims (Rel-15 or 15); checks that require a newer release SKIP, never FAIL (default Rel-18, the SD-Core release; TS 29.244 pinned to Rel-16; \"unset\" runs every check)")
	f.StringVar(&expectedPath, "expected-failures", "",
		"YAML list of this core's known, tolerated FAILs (docs/conformance/expected-failures.md); a listed FAIL still prints FAIL but does not fail the gate")
	f.StringVar(&ueFlags.supi, "supi", "", supiFlagHelp+"; enables the mobility checks")
	f.StringVar(&ueFlags.ki, "ki", "", kiFlagHelp)
	f.StringVar(&ueFlags.opc, "opc", "", opcFlagHelp)
	f.StringVar(&ueFlags.gnbN3, "gnb-n3", "", "gNB N3 IP for the mobility checks' data path (reachable from the UPF; both gNBs use it)")
	f.StringVar(&ueFlags.traffic, "traffic", "", "optional echo peer host:port the mobility checks' UE sends to across the handover")
	_ = cmd.MarkFlagRequired("amf")
	return cmd
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
