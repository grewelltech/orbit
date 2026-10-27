package target

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// Binding maps an env var or a flag to the canonical path it sets
// (ADR-0008 §3). A flag binding applies on the commands listed, or on every
// command that registers the flag when Commands is empty.
type Binding struct {
	Path     string
	Env      string // ORBIT_* name, without $
	Flag     string // flag name, without dashes
	Commands []string
}

// Numeric reports whether the binding's value is a number: its path is an
// integer or float field.
func (b Binding) Numeric() bool {
	_, f, err := lookup(b.Path)
	return err == nil && (f.typ == tUint || f.typ == tFloat)
}

// Reason is why an env var or flag is not configuration.
type Reason string

const (
	// ReasonOutput: how results are shown.
	ReasonOutput Reason = "output"
	// ReasonConnection: how the client reaches the server
	// (~/.config/orbit/servers.yaml, ADR-0008 §4).
	ReasonConnection Reason = "client connection"
	// ReasonProcess: a server or responder process setting; a target's own
	// fields override the server-wide defaults per run.
	ReasonProcess Reason = "server or responder process"
	// ReasonOperation: an argument to an operation on an existing UE or a
	// one-off cell procedure.
	ReasonOperation Reason = "operation argument"
)

// Exclusion is an env var or flag that is not configuration, with why.
type Exclusion struct {
	Env      string
	Flag     string
	Commands []string
	Reason   Reason
}

// conformance, load and the other command paths the table scopes by.
const (
	cmdConformance  = "orbit conformance"
	cmdLoad         = "orbit load"
	cmdStartLoad    = "orbit runs start-load"
	cmdStartFleet   = "orbit runs start-fleet"
	cmdUERegister   = "orbit ue register"
	cmdUEHandover   = "orbit ue handover"
	cmdUEXnHandover = "orbit ue xn-handover"
	cmdCellNGSetup  = "orbit cell ngsetup"
	cmdServe        = "orbit serve"
	cmdTargetInit   = "orbit target init"
	cmdResponder    = "orbit responder"
)

var (
	populationCmds = []string{cmdLoad, cmdStartLoad}
	handoverCmds   = []string{cmdUEHandover, cmdUEXnHandover}
)

// Bindings is the env var and flag → path table.
var Bindings = []Binding{
	// Environment.
	{Env: "ORBIT_TARGET", Path: "target"},
	{Env: "ORBIT_MCC", Path: "core.plmn.mcc"},
	{Env: "ORBIT_MNC", Path: "core.plmn.mnc"},
	{Env: "ORBIT_KI", Path: "subscribers[].ki"},
	{Env: "ORBIT_OPC", Path: "subscribers[].opc"},
	{Env: "ORBIT_LOAD_SEED", Path: "workloads[].attach.seed"},

	// The target.
	{Flag: "target", Path: "target"},
	{Flag: "amf", Path: "core.amf"},
	{Flag: "mcc", Path: "core.plmn.mcc"},
	{Flag: "mnc", Path: "core.plmn.mnc"},
	{Flag: "tac", Path: "core.tac"},
	{Flag: "sst", Path: "core.slices[0].sst"},
	{Flag: "sd", Path: "core.slices[0].sd"},
	{Flag: "dnn", Path: "core.dnn"},
	{Flag: "upf-n3", Path: "core.upf_n3", Commands: []string{cmdConformance}},
	{Flag: "supi", Path: "subscribers[].first_supi", Commands: []string{cmdConformance, cmdUERegister}},
	{Flag: "base-imsi", Path: "subscribers[].first_supi"},
	{Flag: "ki", Path: "subscribers[].ki"},
	{Flag: "opc", Path: "subscribers[].opc"},
	{Flag: "routing-indicator", Path: "subscribers[].routing_indicator"},
	{Flag: "bind", Path: "ran.n2_source_ips", Commands: []string{cmdConformance, cmdLoad, cmdStartLoad, cmdUERegister, cmdCellNGSetup}},
	{Flag: "local", Path: "ran.n2_source_ips", Commands: []string{cmdCellNGSetup}}, // deprecated alias of --bind
	{Flag: "gnb-n3", Path: "ran.n3_ips"},
	{Flag: "n3-bind", Path: "ran.n3_ips", Commands: []string{cmdConformance}},
	{Flag: "gnb-name", Path: "ran.gnb_name"},
	{Flag: "gnb-id", Path: "ran.gnb_ids.first"},
	{Flag: "gnb-base", Path: "ran.gnb_ids.first", Commands: []string{cmdConformance}},
	{Flag: "gnb-id-bits", Path: "ran.gnb_ids.bits"},
	{Flag: "gnb-bits", Path: "ran.gnb_ids.bits"},
	{Flag: "peer", Path: "app.responders[].control"},
	{Flag: "token", Path: "app.responders[].token", Commands: []string{"orbit ue app http", "orbit ue app video", "orbit ue app voip"}},
	{Flag: "peer-data-ip", Path: "app.responders[].data_ip"},

	// The run file.
	{Flag: "name", Path: "run.name", Commands: []string{cmdStartLoad, cmdStartFleet}},
	{Flag: "events", Path: "run.event_verbosity"},
	{Flag: "duration", Path: "until.duration_ms", Commands: populationCmds},
	{Flag: "count", Path: "workloads[].attach.count", Commands: populationCmds},
	{Flag: "rate", Path: "workloads[].attach.rate", Commands: populationCmds},
	{Flag: "ramp", Path: "workloads[].attach.ramp"},
	{Flag: "arrival", Path: "workloads[].attach.arrival"},
	{Flag: "concurrency", Path: "workloads[].attach.concurrency"},
	{Flag: "pdu-session", Path: "workloads[].pdu_session", Commands: append([]string{cmdUERegister}, populationCmds...)},
	{Flag: "gnb-count", Path: "workloads[].topology.gnb_count"},
	{Flag: "sample-interval", Path: "workloads[].sample_interval_ms"},
	{Flag: "slo-reg-p99", Path: "gates[" + GateName("", "slo-reg-p99") + "].slo.threshold"},
	{Flag: "slo-attach-p99", Path: "gates[" + GateName("", "slo-attach-p99") + "].slo.threshold"},
	{Flag: "slo-min-success", Path: "gates[" + GateName("", "slo-min-success") + "].slo.threshold"},
	{Flag: "category", Path: "workloads[].conformance.categories"},
	{Flag: "target-release", Path: "workloads[].conformance.target_release"},
	{Flag: "expected-failures", Path: "workloads[].conformance.expected", Commands: []string{cmdConformance}},
	{Flag: "per-test-timeout", Path: "workloads[].conformance.per_test_timeout_ms"},
	{Flag: "traffic", Path: "workloads[].conformance.traffic", Commands: []string{cmdConformance}},
	{Flag: "nas-integrity", Path: "workloads[].conformance.nas.integrity", Commands: []string{cmdConformance}},
	{Flag: "nas-ciphering", Path: "workloads[].conformance.nas.ciphering", Commands: []string{cmdConformance}},
}

// Exclusions are the env vars and flags that are not configuration.
var Exclusions = []Exclusion{
	{Env: "ORBIT_SERVER", Reason: ReasonConnection},
	{Env: "ORBIT_TOKEN", Reason: ReasonConnection},
	{Flag: "server", Reason: ReasonConnection},

	{Flag: "json", Reason: ReasonOutput},
	{Flag: "explain", Reason: ReasonOutput},
	{Flag: "list", Reason: ReasonOutput},
	{Flag: "out", Reason: ReasonOutput},
	{Flag: "progress-every", Reason: ReasonOutput},
	{Flag: "from-seq", Reason: ReasonOutput},
	{Flag: "wait", Reason: ReasonOutput}, // follow the run to its end and exit with its result
	{Flag: "finish", Commands: []string{"orbit runs stop"}, Reason: ReasonOperation},

	{Flag: "listen", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "tls", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "tls-cert", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "tls-key", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "token-file", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "no-auth", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "insecure-listen", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "fingerprint", Commands: []string{"orbit server trust"}, Reason: ReasonConnection},
	{Flag: "token-file", Commands: []string{"orbit server trust"}, Reason: ReasonConnection},
	{Flag: "log-level", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "state-dir", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "run-history", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "core-profile", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "loom-agent", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "loom-token", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "conformance-expected-failures", Commands: []string{cmdServe}, Reason: ReasonProcess},
	{Flag: "bind", Commands: []string{cmdResponder}, Reason: ReasonProcess},
	{Flag: "token", Commands: []string{cmdResponder}, Reason: ReasonProcess},
	{Flag: "max-flows", Commands: []string{cmdResponder}, Reason: ReasonProcess},
	{Flag: "telemetry-interval", Commands: []string{cmdResponder}, Reason: ReasonProcess},
	{Flag: "metrics-listen", Commands: []string{"orbit run"}, Reason: ReasonProcess},

	// Operations on an existing UE, and one-off cell procedures.
	{Flag: "supi", Reason: ReasonOperation},
	{Flag: "pdu-session-id", Reason: ReasonOperation},
	{Flag: "hold", Reason: ReasonOperation},
	{Flag: "timeout", Reason: ReasonOperation},
	{Flag: "name", Commands: []string{cmdCellNGSetup}, Reason: ReasonOperation},
	{Flag: "count", Commands: []string{"orbit ue ping"}, Reason: ReasonOperation},
	{Flag: "dst", Reason: ReasonOperation},
	{Flag: "probes", Reason: ReasonOperation},
	{Flag: "spacing-ms", Reason: ReasonOperation},
	{Flag: "timeout-ms", Reason: ReasonOperation},
	{Flag: "target", Commands: []string{"orbit ue latency", "orbit ue traffic"}, Reason: ReasonOperation},
	{Flag: "duration-ms", Reason: ReasonOperation},
	{Flag: "packet-size", Reason: ReasonOperation},
	{Flag: "rate", Commands: []string{"orbit ue traffic"}, Reason: ReasonOperation},
	{Flag: "duration", Commands: []string{"orbit ue app http", "orbit ue app video", "orbit ue app voip"}, Reason: ReasonOperation},
	{Flag: "h2", Reason: ReasonOperation},
	{Flag: "host", Reason: ReasonOperation},
	{Flag: "object-size", Reason: ReasonOperation},
	{Flag: "objects", Reason: ReasonOperation},
	{Flag: "port-min", Reason: ReasonOperation},
	{Flag: "port-max", Reason: ReasonOperation},
	{Flag: "think", Reason: ReasonOperation},
	{Flag: "tls", Reason: ReasonOperation},
	{Flag: "tls-ca", Reason: ReasonOperation},
	{Flag: "tls-insecure", Reason: ReasonOperation},
	{Flag: "url-path", Reason: ReasonOperation},
	{Flag: "url-name", Reason: ReasonOperation},
	{Flag: "abr", Reason: ReasonOperation},
	{Flag: "buffer-target", Reason: ReasonOperation},
	{Flag: "ladder", Reason: ReasonOperation},
	{Flag: "rebuffer-target", Reason: ReasonOperation},
	{Flag: "seg-duration", Reason: ReasonOperation},
	{Flag: "segments", Reason: ReasonOperation},
	{Flag: "start-threshold", Reason: ReasonOperation},
	{Flag: "codec", Reason: ReasonOperation},
	{Flag: "jb", Reason: ReasonOperation},
	{Flag: "ptime", Reason: ReasonOperation},
	// target init's flags are the new target document's fields, not
	// settings a target supplies; show --resolved chooses what to print.
	{Flag: "amf", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "mcc", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "mnc", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "tac", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "sst", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "sd", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "dnn", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "ki", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "opc", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "count", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "first-supi", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "write-keys", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "bind", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "gnb-n3", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "no-probe", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "here", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "force", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "responder", Commands: []string{cmdTargetInit}, Reason: ReasonOperation},
	{Flag: "resolved", Commands: []string{"orbit target show"}, Reason: ReasonOutput},
	// A handover's flags describe its destination gNB, with unset fields
	// inherited from the serving gNB: one operation's argument, which a
	// target never supplies. --amf stays bound: the destination gNB
	// connects to the target's core.
	{Flag: "bind", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "gnb-id", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "gnb-bits", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "gnb-n3", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "gnb-name", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "mcc", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "mnc", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "tac", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "sst", Commands: handoverCmds, Reason: ReasonOperation},
	{Flag: "sd", Commands: handoverCmds, Reason: ReasonOperation},
}

// appliesTo reports whether a command-scoped entry covers cmd.
func appliesTo(commands []string, cmd string) bool {
	if len(commands) == 0 {
		return true
	}
	for _, c := range commands {
		if c == cmd {
			return true
		}
	}
	return false
}

// FlagBinding is the binding of flag on command cmd, if any. A binding
// scoped to cmd wins over an unscoped one.
func FlagBinding(cmd, flag string) (Binding, bool) {
	var general *Binding
	for i, b := range Bindings {
		if b.Flag != flag {
			continue
		}
		if len(b.Commands) > 0 && appliesTo(b.Commands, cmd) {
			return b, true
		}
		if len(b.Commands) == 0 && general == nil {
			general = &Bindings[i]
		}
	}
	if general != nil && !excludedScoped(cmd, flag) {
		return *general, true
	}
	return Binding{}, false
}

// excludedScoped reports whether an exclusion names flag on cmd
// specifically, which beats an unscoped binding.
func excludedScoped(cmd, flag string) bool {
	for _, e := range Exclusions {
		if e.Flag == flag && len(e.Commands) > 0 && appliesTo(e.Commands, cmd) {
			return true
		}
	}
	return false
}

// FlagExclusion is the exclusion of flag on command cmd, if any.
func FlagExclusion(cmd, flag string) (Exclusion, bool) {
	if _, ok := FlagBinding(cmd, flag); ok {
		return Exclusion{}, false
	}
	for _, e := range Exclusions {
		if e.Flag == flag && appliesTo(e.Commands, cmd) {
			return e, true
		}
	}
	return Exclusion{}, false
}

// Covered reports whether env var name (without $) is bound or excluded.
func Covered(env string) bool {
	for _, b := range Bindings {
		if b.Env == env {
			return true
		}
	}
	for _, e := range Exclusions {
		if e.Env == env {
			return true
		}
	}
	return false
}

// EnvInput is the env layer: every bound ORBIT_* variable that is set.
func EnvInput(env Getenv) Input {
	in := Input{Layer: LayerEnv}
	for _, b := range Bindings {
		if b.Env == "" {
			continue
		}
		if v, ok := env(b.Env); ok {
			in.Settings = append(in.Settings, Setting{Path: b.Path, Value: v, Source: Source{Name: "$" + b.Env}})
		}
	}
	return in
}

// FlagInput is the flag layer for command cmd: every bound flag the user
// set (pflag's Changed), so a flag's default never outranks a lower layer;
// the defaults are the default layer's. --sst and --sd set the first
// slice's fields, core.slices[0].sst and .sd, on top of the merged list.
func FlagInput(cmd string, fs *pflag.FlagSet) (Input, error) {
	in := Input{Layer: LayerFlag}
	var err error
	fs.Visit(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		b, ok := FlagBinding(cmd, f.Name)
		if !ok {
			return
		}
		src := Source{Name: "--" + f.Name}
		val := any(f.Value.String())
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			val = sv.GetSlice()
		}
		if f.Value.Type() == "duration" && strings.HasSuffix(b.Path, "_ms") {
			// A duration flag (--per-test-timeout 15s) sets a millisecond
			// field.
			// field. The commands treat a duration of zero or less as unset,
			// so a negative one is 0 here: the resolver never refuses what a
			// command accepts.
			d, perr := time.ParseDuration(f.Value.String())
			if perr != nil {
				err = fmt.Errorf("--%s: %q is not a duration", f.Name, f.Value.String())
				return
			}
			val = uint64(max(d, 0).Milliseconds())
		}
		in.Settings = append(in.Settings, Setting{Path: b.Path, Value: val, Source: src})
	})
	if err != nil {
		return Input{}, err
	}
	return in, nil
}

// DefaultInput is the built-in default layer: the CLI's defaults for the
// fields that have a sane one (the PLMN has none).
func DefaultInput() Input {
	b := Source{Name: "built-in"}
	return Input{Layer: LayerDefault, Settings: []Setting{
		{Path: "core.tac", Value: uint64(1), Source: b},
		{Path: "core.slices", Value: []Slice{{SST: 1}}, Source: b},
		{Path: "core.dnn", Value: "internet", Source: b},
		{Path: "core.profile", Value: "strict-3gpp", Source: b},
	}}
}
