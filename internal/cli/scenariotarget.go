package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
)

// targetFileAnnotation marks a command that takes its target through a
// scenario file (orbit run, runs start-fleet): it resolves -t,
// $ORBIT_TARGET and the file's target: itself, so the flag hook
// (targets.apply) leaves it alone.
const targetFileAnnotation = "orbit/target-file"

func consumesTarget(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[targetFileAnnotation] = "true"
}

// scenarioTarget picks and resolves the target of the scenario at path
// (ADR-0008): -t, then $ORBIT_TARGET, then the file's own target: (a name,
// or a document inline). An override of the file's target is reported on
// stderr. A file that sets its own core, credentials or addresses and names
// no target is self-contained: -t is then an error, $ORBIT_TARGET is
// ignored with a notice, and the result is nil. A file with neither is an
// error. The target is resolved over the built-in defaults and $ORBIT_*,
// and printed with its provenance under --explain.
func scenarioTarget(cmd *cobra.Command, path string, data []byte, o options) (*target.Target, error) {
	ref, err := scenario.ReadTargetRef(data)
	if err != nil {
		return nil, err
	}
	own, err := scenario.SelfContained(data)
	if err != nil {
		return nil, err
	}
	file := filepath.Base(path)
	selfContained := len(own) > 0 && !ref.Set()
	errOut := cmd.ErrOrStderr()

	flag := ""
	if f := cmd.Flags().Lookup("target"); f != nil && f.Changed {
		flag = strings.TrimSpace(f.Value.String())
	}
	env := strings.TrimSpace(os.Getenv("ORBIT_TARGET"))
	fileTarget := ref.Name
	if ref.Inline != nil {
		fileTarget = "(inline)"
	}

	var name, from string
	switch {
	case flag != "":
		if selfContained {
			return nil, fmt.Errorf("-t %s: %s sets its own %s; drop them to use a target, or drop -t", flag, file, strings.Join(own, ", "))
		}
		name, from = flag, "flag -t"
	case env != "" && selfContained:
		_, _ = fmt.Fprintf(errOut, "$ORBIT_TARGET=%s ignored: %s has its own core/credentials\n", env, file)
		return nil, nil
	case env != "":
		name, from = env, "$ORBIT_TARGET"
	case ref.Name != "":
		name, from = ref.Name, "the file's target:"
	case ref.Inline != nil:
		name, from = "(inline)", "the file's inline target:"
	case selfContained:
		return nil, nil
	default:
		return nil, fmt.Errorf("%s names no target: add target: <name> or pass -t", file)
	}
	if (from == "flag -t" || from == "$ORBIT_TARGET") && ref.Set() && name != ref.Name {
		_, _ = fmt.Fprintf(errOut, "target %s (%s) overrides the file's target: %s\n", name, from, fileTarget)
	}

	var f *target.File
	if name == "(inline)" {
		files, err := target.Parse(path+" (inline target)", ref.Inline, os.LookupEnv)
		if err != nil {
			return nil, err
		}
		f = files[0]
	} else {
		loc := o.locations
		if loc == nil {
			l, err := target.DefaultLocations()
			if err != nil {
				return nil, err
			}
			loc = &l
		}
		if f, err = target.Find(name, *loc, os.LookupEnv); err != nil {
			return nil, err
		}
	}
	r, err := target.Resolve(target.DefaultInput(), f.Input, target.EnvInput(os.LookupEnv))
	if err != nil {
		return nil, err
	}
	if e := cmd.Flags().Lookup("explain"); e != nil && e.Value.String() == "true" {
		_, _ = fmt.Fprintf(errOut, "%s: target %s (%s), resolved: default < target < env\n", file, f.Target.Name, from)
		var rows []target.FieldProvenance
		for _, p := range r.Provenance {
			if !documentField(p.Path) {
				rows = append(rows, p)
			}
		}
		if err := printProvenance(errOut, rows); err != nil {
			return nil, err
		}
	}
	t := r.Target
	return &t, nil
}
