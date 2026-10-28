package plan

import (
	"errors"
	"fmt"
	"reflect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/target"
)

// Documents are a run as a client sends it whole (ADR-0008 §3): the
// target document and the run file as text, the template the run file
// started from, the flag layer, and the values of the ${VAR}s they name.
// There is no env layer: a ${VAR} takes its value from Vars or is an
// error naming it, never from this process's environment.
type Documents struct {
	TargetYAML []byte
	RunYAML    []byte
	Template   string
	Sets       []string
	Gates      []string
	Vars       map[string]string
}

// Planned is a run built from Documents: the StartRun request (its plan's
// provenance filled, as the CLI fills it) and the resolution it came from.
type Planned struct {
	Request  *orbitv1.StartRunRequest
	Resolved *target.Resolved
}

// FromDocuments resolves d (defaults < target < run file < flags) and
// builds the plan, as `orbit run` and `orbit test` build theirs from the
// same files. It reads no file: an expected-failures list must be inline.
// A template's fill from the target (the fleet's latency probe) applies
// only while the run file leaves that field as the template has it, as
// `orbit test` fills only what --set does not set.
func FromDocuments(d Documents) (*Planned, error) {
	getenv := func(name string) (string, bool) { v, ok := d.Vars[name]; return v, ok }
	fs, err := target.Parse("target", d.TargetYAML, getenv)
	if err != nil {
		return nil, err
	}
	if len(fs) != 1 {
		return nil, fmt.Errorf("target: want one target document, not %d", len(fs))
	}
	tf := fs[0]
	if tf.Err != nil {
		return nil, tf.Err
	}
	rf := &target.RunFile{Input: target.Input{Layer: target.LayerRunFile}}
	if len(d.RunYAML) > 0 {
		if rf, err = target.ParseRunFile("run file", d.RunYAML, getenv); err != nil {
			return nil, err
		}
	}
	if rf.Target != "" && rf.Target != tf.Target.Name {
		return nil, fmt.Errorf("the run file's target: is %q, not the target sent (%q)", rf.Target, tf.Target.Name)
	}
	var fill func(*target.Resolved) bool
	if d.Template != "" {
		base, err := Template(d.Template)
		if err != nil {
			return nil, err
		}
		fill = func(res *target.Resolved) bool {
			if !sameAsTemplate(rf, base, fleetBehaviors) {
				return false
			}
			changed, _ := FillTemplate(d.Template, rf, res.Target)
			return changed
		}
	}
	flags, err := flagSettings(d.Sets, d.Gates)
	if err != nil {
		return nil, err
	}
	build := func() (*Planned, error) {
		res, err := target.Resolve(target.DefaultInput(), tf.Input, rf.Input, target.Input{Layer: target.LayerFlag, Settings: flags})
		if err != nil {
			return nil, err
		}
		if len(d.RunYAML) == 0 {
			return &Planned{Resolved: res}, nil
		}
		p, prov, err := Build(res)
		if err != nil {
			return nil, err
		}
		p.Provenance = prov
		verbosity, err := EventVerbosity(res)
		if err != nil {
			return nil, err
		}
		return &Planned{
			Request:  &orbitv1.StartRunRequest{Name: RunName(res), Spec: &orbitv1.StartRunRequest_Plan{Plan: p}, EventVerbosity: verbosity},
			Resolved: res,
		}, nil
	}
	out, err := build()
	if err != nil {
		return nil, err
	}
	if fill != nil && fill(out.Resolved) {
		return build()
	}
	return out, nil
}

// sameAsTemplate reports whether rf sets path as base does.
func sameAsTemplate(rf, base *target.RunFile, path string) bool {
	find := func(in target.Input) (any, bool) {
		for _, s := range in.Settings {
			if s.Path == path {
				return s.Value, true
			}
		}
		return nil, false
	}
	a, okA := find(rf.Input)
	b, okB := find(base.Input)
	return okA && okB && reflect.DeepEqual(a, b)
}

// ErrNoRun is a Documents with no run file asked for a plan.
var ErrNoRun = errors.New("no run file: a plan needs one")
