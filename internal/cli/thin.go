package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/target"
)

// thinFlags are the flags of a command that runs on the server by default
// (ADR-0008 §6): --local runs it in this process instead, --detach starts
// it and returns.
type thinFlags struct {
	local, detach bool
}

const detachHelp = "start the run on the server, print its ID and return instead of waiting for its end"

func (t *thinFlags) add(fs *pflag.FlagSet, localHelp string) {
	fs.BoolVar(&t.local, "local", false, localHelp)
	fs.BoolVar(&t.detach, "detach", false, detachHelp)
}

// localDeprecated is the notice a deprecated --local prints.
const localDeprecated = "--local is deprecated and goes after this release: run `orbit serve` and drop --local"

// localFleetDeprecated is orbit run's: a fleet's cohort gauges
// (--metrics-listen) need --local until orbit serve's /metrics serves them
// (the follow-up "serve fleet app metrics from orbit serve's /metrics"),
// so --local stays until then.
const localFleetDeprecated = "--local is deprecated: run `orbit serve` and drop --local. A fleet's cohort gauges " +
	"(--metrics-listen) need --local until orbit serve's /metrics serves them, and --local stays until then"

// noServer turns a failure to reach the server into the error that says
// what to do; any other error is returned as is.
func noServer(err error, url string) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return exitWith(ExitCouldNotStart, fmt.Errorf("no orbit server at %s: start one with `orbit serve`, or pass --local", url))
	}
	return err
}

// thinPlan builds the run a command's flags describe as a plan, through
// the same layers and plan.Build as a run file (ADR-0008 §3): the built-in
// defaults, base, the command's own flag defaults, the target (-t, else
// $ORBIT_TARGET), the environment, and the flags the user typed, then
// extra. A flag a target filled (setFromTarget) is the target's value, so
// it comes from the target's layer, not the flags'. base names the
// command's workload (and any gate it always has), and so goes first.
// skip names flags whose bound path the command maps another way (in
// extra).
func thinPlan(cmd *cobra.Command, o options, base, extra []target.Setting, skip ...string) (*orbitv1.StartRunRequest, error) {
	path := cmd.CommandPath()
	fs := cmd.Flags()
	inputs := []target.Input{target.DefaultInput(), {Layer: target.LayerDefault, Settings: base}}
	defaults, err := target.DefaultsInput(path, fs)
	if err != nil {
		return nil, err
	}
	inputs = append(inputs, defaults)
	name := ""
	if v, ok := os.LookupEnv("ORBIT_TARGET"); ok {
		name = strings.TrimSpace(v)
	}
	if f := fs.Lookup("target"); f != nil && f.Changed {
		name = strings.TrimSpace(f.Value.String())
	}
	if name != "" {
		file, err := findTarget(o, name)
		if err != nil {
			return nil, err
		}
		if file.Err != nil {
			return nil, file.Err
		}
		inputs = append(inputs, file.Input)
	}
	flags, err := target.FlagInput(path, fs)
	if err != nil {
		return nil, err
	}
	typed := target.Input{Layer: target.LayerFlag}
	for _, s := range flags.Settings {
		name := strings.TrimPrefix(s.Source.Name, "--")
		if f := fs.Lookup(name); slices.Contains(skip, name) || (f != nil && len(f.Annotations[fromTargetAnnotation]) > 0) {
			continue
		}
		typed.Settings = append(typed.Settings, s)
	}
	typed.Settings = append(typed.Settings, extra...)
	inputs = append(inputs, withoutTarget(target.EnvInput(os.LookupEnv)), typed)
	res, err := target.Resolve(inputs...)
	if err != nil {
		return nil, err
	}
	p, prov, err := plan.Build(res, plan.WithReadFile(os.ReadFile))
	if err != nil {
		return nil, err
	}
	p.Provenance = prov
	verbosity, err := plan.EventVerbosity(res)
	if err != nil {
		return nil, err
	}
	return &orbitv1.StartRunRequest{Name: plan.RunName(res), Spec: &orbitv1.StartRunRequest_Plan{Plan: p}, EventVerbosity: verbosity}, nil
}

// thinRun starts req on the server and, unless detach, follows it to its
// end (waitRunWith, with opts) and exits with its result. The run's ID goes
// to stdout when detached (scripts capture it), else to stderr beside the
// command's own output. A server that is not there says what to do.
func thinRun(cmd *cobra.Command, url string, req *orbitv1.StartRunRequest, detach bool, opts waitOpts) error {
	c := runsClient(&url)
	var sigs <-chan os.Signal
	if !detach {
		ch, stop := waitSignalsFn()
		defer stop()
		sigs = ch
	}
	res, err := c.StartRun(cmd.Context(), connect.NewRequest(req))
	if err != nil {
		return noServer(exitWith(ExitCouldNotStart, err), url)
	}
	id := res.Msg.GetRun().GetRunId()
	if opts.started != nil {
		opts.started()
	}
	if detach {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), id)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "run %s on %s; Ctrl-C finishes it, a second Ctrl-C cancels it\n", id, url)
	if err := waitRunWith(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), c, id, sigs, opts); err != nil {
		var typed *ExitError
		if errors.As(err, &typed) {
			return err
		}
		return exitWith(ExitDidNotComplete, err)
	}
	return nil
}

// conformanceClient builds a ConformanceService client against the API
// server.
func conformanceClient(url string) orbitv1connect.ConformanceServiceClient {
	h, guard := apiClient(url)
	return orbitv1connect.NewConformanceServiceClient(h, url, guard)
}
