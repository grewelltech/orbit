package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/target"
)

// gateFlags are the gate flags every run-starting command takes: --gate,
// and the --slo-* aliases for gates on the server's metrics.
type gateFlags struct {
	specs                   []string
	sloRegP99, sloAttachP99 time.Duration
	sloMinSuccess           float64
}

func (g *gateFlags) add(fs *pflag.FlagSet) {
	fs.StringArrayVar(&g.specs, "gate", nil, gateFlagHelp)
	fs.DurationVar(&g.sloRegP99, "slo-reg-p99", 0, "gate: max registration P99 (e.g. 500ms); --gate 'registration.p99_ms<=500ms'")
	fs.DurationVar(&g.sloAttachP99, "slo-attach-p99", 0, "gate: max attach P99; --gate 'attach.p99_ms<=…'")
	fs.Float64Var(&g.sloMinSuccess, "slo-min-success", 0, "gate: minimum attach success ratio (0-1); --gate 'attach.success_ratio>=…'")
}

// settings are the gates' fields, in the flag layer: --gate's, then the
// --slo-* aliases' (bound to their gates' thresholds in the mapping).
func (g *gateFlags) settings(cmd *cobra.Command) ([]target.Setting, error) {
	var out []target.Setting
	for _, spec := range g.specs {
		s, err := plan.ParseGate(spec)
		if err != nil {
			return nil, exitWith(ExitCouldNotStart, err)
		}
		out = append(out, s...)
	}
	in, err := target.FlagInput(cmd.CommandPath(), cmd.Flags())
	if err != nil {
		return nil, exitWith(ExitCouldNotStart, err)
	}
	for _, s := range in.Settings {
		if strings.HasPrefix(s.Path, "gates[") {
			out = append(out, s)
		}
	}
	return out, nil
}

// gates builds the gates the flags give, for a command whose plan is
// built another way; none when no gate flag is set.
func (g *gateFlags) gates(cmd *cobra.Command) ([]*orbitv1.Gate, error) {
	s, err := g.settings(cmd)
	if err != nil || len(s) == 0 {
		return nil, err
	}
	res, err := target.Resolve(target.Input{Layer: target.LayerFlag, Settings: s})
	if err != nil {
		return nil, exitWith(ExitCouldNotStart, err)
	}
	out, err := plan.Gates(res)
	if err != nil {
		return nil, exitWith(ExitCouldNotStart, err)
	}
	return out, nil
}

// planFlags are the flags every plan-building command takes: gates and
// fields on top of the run file or template.
type planFlags struct {
	gateFlags
	sets []string
	dur  time.Duration
}

const (
	gateFlagHelp = "add a gate: [name=]metric<op>value (e.g. registration.p99_ms<=500ms, attach.success_ratio>=0.99), " +
		"or conformance; repeat for more"
	setFlagHelp = "set a run-file field: path=value (e.g. workloads[load].attach.count=500); repeat for more. " +
		"A whole value (behaviors, start, conformance.expected) is set whole; to change inside one, write it in a run file"
)

func (f *planFlags) add(fs *pflag.FlagSet) {
	f.gateFlags.add(fs)
	fs.StringArrayVar(&f.sets, "set", nil, setFlagHelp)
}

// settings are the flags' fields, the flag layer of the plan: --gate's,
// --set's, and --for's, which replaces however the lower layers stop the
// run.
func (f *planFlags) settings(cmd *cobra.Command) ([]target.Setting, error) {
	out, err := f.gateFlags.settings(cmd)
	if err != nil {
		return nil, err
	}
	for _, kv := range f.sets {
		path, value, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(path) == "" {
			return nil, exitWith(ExitCouldNotStart, fmt.Errorf("--set %q: want path=value", kv))
		}
		out = append(out, target.Setting{Path: strings.TrimSpace(path), Value: value, Source: target.Source{Name: "--set"}})
	}
	if f.dur > 0 {
		out = append(out, plan.ForSettings(f.dur)...)
	}
	return out, nil
}

// buildPlan resolves a run file (or template) over its target and the
// higher layers, and builds the StartRun request (plan.Build). The target
// is -t, else $ORBIT_TARGET, else the run file's target:. dir is where a
// file the run file names (an expected-failures list) is read from.
func buildPlan(cmd *cobra.Command, o options, rf *target.RunFile, flags []target.Setting, dir string) (*orbitv1.StartRunRequest, *target.Resolved, string, error) {
	name := rf.Target
	if env := strings.TrimSpace(os.Getenv("ORBIT_TARGET")); env != "" {
		name = env
	}
	if f := cmd.Flags().Lookup("target"); f != nil && f.Changed {
		name = strings.TrimSpace(f.Value.String())
	}
	if name == "" {
		return nil, nil, "", errors.New("a run needs a target: -t <name>, $ORBIT_TARGET, or a target: line in the run file")
	}
	file, err := findTarget(o, name)
	if err != nil {
		return nil, nil, "", err
	}
	if file.Err != nil {
		return nil, nil, "", file.Err
	}
	env := target.EnvInput(os.LookupEnv)
	env = withoutTarget(env)
	res, err := target.Resolve(target.DefaultInput(), file.Input, rf.Input, env, target.Input{Layer: target.LayerFlag, Settings: flags})
	if err != nil {
		return nil, nil, "", err
	}
	read := func(p string) ([]byte, error) {
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return os.ReadFile(p)
	}
	p, prov, err := plan.Build(res, plan.WithReadFile(read))
	if err != nil {
		return nil, nil, "", err
	}
	p.Provenance = prov
	verbosity, err := plan.EventVerbosity(res)
	if err != nil {
		return nil, nil, "", err
	}
	return &orbitv1.StartRunRequest{Name: plan.RunName(res), Spec: &orbitv1.StartRunRequest_Plan{Plan: p}, EventVerbosity: verbosity},
		res, name, nil
}

// findTarget loads target name from the configured locations.
func findTarget(o options, name string) (*target.File, error) {
	loc := o.locations
	if loc == nil {
		l, err := target.DefaultLocations()
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", name, err)
		}
		loc = &l
	}
	return target.Find(name, *loc, os.LookupEnv)
}

// explainPlan prints, for --explain, where each field of the plan came
// from (secrets and secret-bearing subtrees masked) and the plan itself as
// the server would receive it, keys masked. Nothing is started.
func explainPlan(cmd *cobra.Command, targetName string, res *target.Resolved, req *orbitv1.StartRunRequest) error {
	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "%s: target %s\n", cmd.CommandPath(), targetName)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FIELD\tVALUE\tSET BY\tOVERRODE")
	for _, pv := range res.Provenance {
		var over []string
		for _, o := range pv.Overridden {
			over = append(over, shortSource(o.Source)+"="+o.Value)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", pv.Path, pv.Value, shortSource(pv.Source), dash(strings.Join(over, "; ")))
	}
	_ = tw.Flush()
	printNotices(cmd, res)
	masked := proto.Clone(req.GetPlan())
	secrets.Mask(masked)
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(masked)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", b)
	return nil
}

// runPlanFile runs a kind: run file on the server: resolved over its target,
// built (plan.Build) and started; it waits for the run's end unless detach.
func runPlanFile(cmd *cobra.Command, serverURL *string, o options, path string, data []byte, pf planFlags, detach bool) error {
	if cmd.Flags().Changed("metrics-listen") {
		return errors.New("--metrics-listen applies to an in-process fleet scenario; a run file runs on the server, which serves its own metrics")
	}
	rf, err := target.ParseRunFile(path, data, os.LookupEnv)
	if err != nil {
		return exitWith(ExitCouldNotStart, err)
	}
	flags, err := pf.settings(cmd)
	if err != nil {
		return err
	}
	req, res, name, err := buildPlan(cmd, o, rf, flags, filepath.Dir(path))
	if err != nil {
		return exitWith(ExitCouldNotStart, err)
	}
	if explaining(cmd) {
		return explainPlan(cmd, name, res, req)
	}
	if err := applyServerURL(cmd, serverURL); err != nil {
		return err
	}
	if !detach {
		printNotices(cmd, res)
	}
	return startRun(cmd, runsClient(serverURL), req, !detach, false)
}

// printNotices prints, to stderr, each field an env var set over a file's
// value (plan.OverrideNotices; ADR-0008 §3): a forgotten export in the
// shell, beating the target or the run file. --explain and a waited-for
// start print them; the run's archive keeps the provenance they come from.
func printNotices(cmd *cobra.Command, res *target.Resolved) {
	for _, n := range plan.OverrideNotices(plan.ProvenanceProto(res.Provenance)) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "notice: %s\n", plan.NoticeText(n))
	}
}

// explaining reports whether the root's --explain is set.
func explaining(cmd *cobra.Command) bool {
	e := cmd.Flags().Lookup("explain")
	return e != nil && e.Value.String() == "true"
}
