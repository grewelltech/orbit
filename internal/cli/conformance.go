package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/gnb"
)

// newConformanceCmd runs the conformance / regression suite in-process against
// a live core and reports structured results. It exits non-zero if any check
// fails or the harness errors — an integration-CI gate.
func newConformanceCmd() *cobra.Command {
	var (
		amf, mcc, mnc string
		upfN3, n3Bind string
		gnbBase       uint32
		categories    []string
		jsonOut       bool
		perTest       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run the core conformance / regression suite against a live core",
		RunE: func(cmd *cobra.Command, args []string) error {
			env := conformance.Env{
				AMFAddr: amf,
				UPFN3:   upfN3,
				N3Bind:  n3Bind,
				GNB: gnb.Config{
					ID: gnbBase, Name: "orbit-conf", MCC: mcc, MNC: mnc, TAC: 1,
					Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}},
				},
			}
			cats := make([]conformance.Category, 0, len(categories))
			for _, c := range categories {
				cats = append(cats, conformance.Category(c))
			}
			results := conformance.NewRegistry().Run(cmd.Context(), env, perTest, cats...)
			sum := conformance.Summarize(results)

			out := cmd.OutOrStdout()
			if jsonOut {
				b, _ := json.MarshalIndent(sum, "", "  ")
				fmt.Fprintln(out, string(b))
			} else {
				for _, r := range sum.Results {
					fmt.Fprintf(out, "%-5s [%-11s] %-26s %s\n", r.Verdict, r.Category, r.ID, r.Observed)
					if r.Detail != "" {
						fmt.Fprintf(out, "      %s — %s\n", r.SpecRef, r.Detail)
					}
				}
				fmt.Fprintf(out, "\n%d checks: %d pass, %d fail, %d deviation, %d error, %d skip\n",
					sum.Total, sum.Pass, sum.Fail, sum.Deviation, sum.Error, sum.Skip)
			}
			if !sum.OK() {
				return fmt.Errorf("conformance gate failed: %d fail, %d error", sum.Fail, sum.Error)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 endpoint (host:port)")
	f.StringVar(&mcc, "mcc", "208", "MCC")
	f.StringVar(&mnc, "mnc", "93", "MNC")
	f.StringVar(&upfN3, "upf-n3", "", "UPF N3 endpoint host:port (enables GTP-U checks; run from the UPF access network)")
	f.StringVar(&n3Bind, "n3-bind", "", "local N3 source host:port for GTP-U checks (e.g. 172.17.50.12:2152)")
	f.Uint32Var(&gnbBase, "gnb-base", 0x400, "base gNB ID (each check uses a distinct one)")
	f.StringSliceVar(&categories, "category", nil, "only run these categories (procedural, negative-ie, security, gtpu, timing)")
	f.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	f.DurationVar(&perTest, "per-test-timeout", 15*time.Second, "per-check timeout")
	_ = cmd.MarkFlagRequired("amf")
	return cmd
}
