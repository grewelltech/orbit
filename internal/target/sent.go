package target

import (
	"errors"
	"fmt"
	"strings"
)

// SentSource is the source a target sent whole in a request has: the
// target layer, named for where it came from.
var SentSource = Source{Layer: LayerTarget, Name: "the request"}

// SettingsOf is t as target-layer settings: one per field t sets. A zero
// value (0, "", an empty list) is a field not set, as a key absent from a
// file is, so the layer below (the defaults) shows through. That is the
// only reading a sent target allows: its wire form (proto3) cannot tell a
// 0 or "" written on purpose from one never set.
func SettingsOf(t Target, src Source) []Setting {
	var out []Setting
	str := func(p, v string) {
		if v != "" {
			out = append(out, Setting{Path: p, Value: v, Source: src})
		}
	}
	num := func(p string, v uint32) {
		if v != 0 {
			out = append(out, Setting{Path: p, Value: uint64(v), Source: src})
		}
	}
	strs := func(p string, v []string) {
		if len(v) > 0 {
			vals := make([]any, len(v))
			for i, s := range v {
				vals[i] = s
			}
			out = append(out, Setting{Path: p, Value: vals, Source: src})
		}
	}
	str("name", t.Name)
	str("core.amf", t.Core.AMF)
	str("core.plmn.mcc", t.Core.PLMN.MCC)
	str("core.plmn.mnc", t.Core.PLMN.MNC)
	num("core.tac", t.Core.TAC)
	if len(t.Core.Slices) > 0 {
		out = append(out, Setting{Path: "core.slices", Value: append([]Slice(nil), t.Core.Slices...), Source: src})
	}
	str("core.dnn", t.Core.DNN)
	str("core.profile", t.Core.Profile)
	str("core.upf_n3", t.Core.UPFN3)
	strs("core.ue_pools", t.Core.UEPools)
	str("core.upf_n6", t.Core.UPFN6)
	str("core.n6_subnet", t.Core.N6Subnet)
	for _, p := range t.Subscribers {
		b := "subscribers[" + p.Name + "]."
		out = append(out, Setting{Path: b + "name", Value: p.Name, Source: src})
		str(b+"first_supi", p.FirstSUPI)
		num(b+"count", p.Count)
		str(b+"ki", p.Ki)
		str(b+"opc", p.OPc)
		str(b+"routing_indicator", p.RoutingIndicator)
	}
	str("ran.node", t.RAN.Node)
	str("ran.gnb_name", t.RAN.GNBName)
	strs("ran.n2_source_ips", t.RAN.N2SourceIPs)
	strs("ran.n3_ips", t.RAN.N3IPs)
	num("ran.gnb_ids.first", t.RAN.GNBIDs.First)
	num("ran.gnb_ids.count", t.RAN.GNBIDs.Count)
	num("ran.gnb_ids.bits", t.RAN.GNBIDs.Bits)
	for _, r := range t.App.Responders {
		b := "app.responders[" + r.Name + "]."
		out = append(out, Setting{Path: b + "name", Value: r.Name, Source: src})
		str(b+"control", r.Control)
		str(b+"token", r.Token)
		str(b+"data_ip", r.DataIP)
	}
	return out
}

// FieldError is a target field a check refused: the canonical path and
// why, in the words the CLI uses for the same value in a file. Error is
// "<path>: <reason>", the form the dashboard reads the path from.
type FieldError struct {
	Path   string
	Reason string
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Reason }

// ResolveSent resolves a target sent whole, as a dashboard sends one: its
// fields (SettingsOf: a zero value is unset) as the target layer over the
// built-in defaults, with the checks a target file gets when the CLI loads
// it. The first field a check refuses is a *FieldError, its value masked
// where the field is secret; a document rule (distinct names, ran.node) is
// a FieldError too, on the field it is about.
//
// Nothing is expanded: a ${VAR} in a sent field is that text, and a check
// sees it as written.
func ResolveSent(t Target) (*Resolved, error) {
	res, err := resolveSent(t)
	var fe *FieldError
	if errors.As(err, &fe) {
		// Still a *FieldError (the dashboard marks the field from its
		// path), with any key text in a name or value masked.
		return nil, &FieldError{Path: maskKeyShaped(fe.Path), Reason: maskKeyShaped(fe.Reason)}
	}
	return res, maskErr(err)
}

func resolveSent(t Target) (*Resolved, error) {
	if t.Name == "" {
		return nil, &FieldError{Path: "name", Reason: "is required"}
	}
	for _, list := range []struct {
		path  string
		names []string
	}{{"subscribers", poolNames(t.Subscribers)}, {"app.responders", responderNames(t.App.Responders)}} {
		seen := map[string]bool{}
		for i, n := range list.names {
			switch {
			case n == "":
				return nil, &FieldError{Path: fmt.Sprintf("%s[%d].name", list.path, i), Reason: "is required"}
			case strings.ContainsAny(n, ".[]"):
				return nil, &FieldError{Path: fmt.Sprintf("%s[%d].name", list.path, i), Reason: fmt.Sprintf("%q may not contain '.', '[' or ']'", n)}
			case seen[n]:
				return nil, &FieldError{Path: list.path + "[" + n + "].name", Reason: fmt.Sprintf("%q twice; %s need distinct names", n, list.path)}
			}
			seen[n] = true
		}
	}
	if t.RAN.Node != "" {
		return nil, &FieldError{Path: "ran.node", Reason: ErrNodeReserved.Error()}
	}
	res, err := Report(DefaultInput(), Input{Layer: LayerTarget, Settings: SettingsOf(t, SentSource)})
	if err != nil {
		return nil, err
	}
	if len(res.Problems) > 0 {
		p := res.Problems[0]
		reason := strings.TrimPrefix(p.Err, p.Path+": ")
		reason = strings.TrimSuffix(reason, " ("+p.Source.String()+")")
		return nil, &FieldError{Path: p.Path, Reason: reason}
	}
	res.Target.Version, res.Target.Kind = Version, Kind
	return res, nil
}

func poolNames(ps []Pool) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func responderNames(rs []Responder) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}
