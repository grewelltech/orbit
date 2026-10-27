package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/plan"
)

// newTestCmd runs one of the built-in run templates (load, fleet,
// conformance) against a target on the server. The templates are
// address-free: the core, keys and addresses come from the target.
func newTestCmd(serverURL *string, o options) *cobra.Command {
	var (
		pf         planFlags
		wait, list bool
	)
	cmd := &cobra.Command{
		Use:   "test <template> -t <target>",
		Short: "Start a built-in test (load, fleet, conformance) on the server against a target",
		Long: "Start a built-in test on the server against a target, and return once it\n" +
			"has started: orbit test prints \"started <run-id>; follow with\n" +
			"`orbit runs watch <run-id>` or pass --wait\". With --wait it follows the run\n" +
			"to its end and exits with its result: 0 passed, 1 a gate failed, 2 did not\n" +
			"complete, 3 could not start.\n\n" +
			"A template is a run file (docs/run-files.md) with no addresses; the target\n" +
			"(-t or $ORBIT_TARGET) supplies the core, keys and addresses. The load and\n" +
			"fleet templates gate on attach success of at least 0.99; --gate adds a\n" +
			"gate or replaces one of the same name, --set changes any run-file field,\n" +
			"and --for runs for a fixed time instead of the template's stop. --list\n" +
			"lists the templates, --list <template> prints one, and --explain shows the\n" +
			"plan without starting it.",
		Example: "  orbit test load -t lab --wait\n" +
			"  orbit test load -t lab --for 10m --gate 'registration.p99_ms<=500ms'\n" +
			"  orbit test load -t lab --set workloads[load].attach.count=1000\n" +
			"  orbit test conformance -t lab --wait\n" +
			"  orbit test --list load",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				return listTemplates(cmd, args)
			}
			if len(args) == 0 {
				return exitWith(ExitCouldNotStart, fmt.Errorf("name a template: %s (orbit test --list)", strings.Join(plan.Templates(), ", ")))
			}
			rf, err := plan.Template(args[0])
			if err != nil {
				return exitWith(ExitCouldNotStart, err)
			}
			flags, err := pf.settings(cmd)
			if err != nil {
				return err
			}
			// A template names no file, so a relative path in --set (an
			// expected-failures list) is read from the working directory.
			req, res, name, err := buildPlan(cmd, o, rf, flags, ".")
			if err != nil {
				return exitWith(ExitCouldNotStart, err)
			}
			// What the template takes from the target (the fleet's
			// latency probe) is known once the target is resolved.
			changed, note := plan.FillTemplate(args[0], rf, res.Target)
			if note != "" {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			if changed {
				if req, res, name, err = buildPlan(cmd, o, rf, flags, "."); err != nil {
					return exitWith(ExitCouldNotStart, err)
				}
			}
			if explaining(cmd) {
				return explainPlan(cmd, name, res, req)
			}
			if err := applyServerURL(cmd, serverURL); err != nil {
				return err
			}
			return startRun(cmd, runsClient(serverURL), req, wait, true)
		},
	}
	consumesTarget(cmd)
	f := cmd.Flags()
	pf.add(f)
	f.DurationVar(&pf.dur, "for", 0, "run for this long instead of the template's stop (e.g. 10m)")
	f.BoolVar(&wait, "wait", false, waitHelp)
	f.BoolVar(&list, "list", false, "list the templates, or with a template name print it")
	return cmd
}

// listTemplates prints the template names, or one template's text.
func listTemplates(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	if len(args) == 1 {
		text, err := plan.TemplateText(args[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(out, text)
		return nil
	}
	names := plan.Templates()
	if len(names) == 0 {
		return errors.New("no templates are built in")
	}
	for _, n := range names {
		_, _ = fmt.Fprintln(out, n)
	}
	return nil
}
