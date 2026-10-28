package plan

import (
	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/target"
)

// TargetProto is a resolved target as the plan carries it, keys included:
// on the wire only, the server masks them in everything it records.
func TargetProto(t target.Target) *orbitv1.ResolvedTarget {
	out := &orbitv1.ResolvedTarget{
		Name: t.Name,
		Core: &orbitv1.TargetCore{Amf: t.Core.AMF, Plmn: &orbitv1.Plmn{Mcc: t.Core.PLMN.MCC, Mnc: t.Core.PLMN.MNC},
			Tac: t.Core.TAC, Dnn: t.Core.DNN, Profile: t.Core.Profile, UpfN3: t.Core.UPFN3,
			UePools: t.Core.UEPools, UpfN6: t.Core.UPFN6, N6Subnet: t.Core.N6Subnet},
		Ran: &orbitv1.TargetRAN{Node: t.RAN.Node, GnbName: t.RAN.GNBName, N2SourceIps: t.RAN.N2SourceIPs, N3Ips: t.RAN.N3IPs,
			GnbIds: &orbitv1.GNBIDRange{First: t.RAN.GNBIDs.First, Count: t.RAN.GNBIDs.Count, Bits: t.RAN.GNBIDs.Bits}},
		App: &orbitv1.TargetApp{},
	}
	for _, s := range t.Core.Slices {
		out.Core.Slices = append(out.Core.Slices, &orbitv1.Snssai{Sst: s.SST, Sd: s.SD})
	}
	for _, p := range t.Subscribers {
		out.Subscribers = append(out.Subscribers, &orbitv1.SubscriberPool{Name: p.Name, FirstSupi: p.FirstSUPI, Count: p.Count,
			Ki: p.Ki, Opc: p.OPc, RoutingIndicator: p.RoutingIndicator})
	}
	for _, r := range t.App.Responders {
		out.App.Responders = append(out.App.Responders, &orbitv1.Responder{Name: r.Name, Control: r.Control, Token: r.Token, DataIp: r.DataIP})
	}
	return out
}

// TargetFromProto is TargetProto's inverse: a target sent on the wire as
// the target it describes, keys included. Version and kind are the
// schema's: the wire form does not carry them.
func TargetFromProto(rt *orbitv1.ResolvedTarget) target.Target {
	core, ran := rt.GetCore(), rt.GetRan()
	t := target.Target{
		Version: target.Version, Kind: target.Kind, Name: rt.GetName(),
		Core: target.Core{AMF: core.GetAmf(), PLMN: target.PLMN{MCC: core.GetPlmn().GetMcc(), MNC: core.GetPlmn().GetMnc()},
			TAC: core.GetTac(), DNN: core.GetDnn(), Profile: core.GetProfile(), UPFN3: core.GetUpfN3(),
			UEPools: core.GetUePools(), UPFN6: core.GetUpfN6(), N6Subnet: core.GetN6Subnet()},
		RAN: target.RAN{Node: ran.GetNode(), GNBName: ran.GetGnbName(), N2SourceIPs: ran.GetN2SourceIps(), N3IPs: ran.GetN3Ips(),
			GNBIDs: target.GNBIDs{First: ran.GetGnbIds().GetFirst(), Count: ran.GetGnbIds().GetCount(), Bits: ran.GetGnbIds().GetBits()}},
	}
	for _, s := range core.GetSlices() {
		t.Core.Slices = append(t.Core.Slices, target.Slice{SST: s.GetSst(), SD: s.GetSd()})
	}
	for _, p := range rt.GetSubscribers() {
		t.Subscribers = append(t.Subscribers, target.Pool{Name: p.GetName(), FirstSUPI: p.GetFirstSupi(), Count: p.GetCount(),
			Ki: p.GetKi(), OPc: p.GetOpc(), RoutingIndicator: p.GetRoutingIndicator()})
	}
	for _, r := range rt.GetApp().GetResponders() {
		t.App.Responders = append(t.App.Responders, target.Responder{Name: r.GetName(), Control: r.GetControl(), Token: r.GetToken(), DataIP: r.GetDataIp()})
	}
	return t
}

// ProvenanceProto is the resolver's provenance as the plan carries it:
// informational, values already masked where secret (a whole subtree with
// a secret in it, too); the server's path guard masks again.
func ProvenanceProto(ps []target.FieldProvenance) []*orbitv1.FieldProvenance {
	out := make([]*orbitv1.FieldProvenance, 0, len(ps))
	for _, p := range ps {
		fp := &orbitv1.FieldProvenance{Path: p.Path, Source: sourceProto(p.Source), Value: p.Value}
		for _, o := range p.Overridden {
			fp.Overridden = append(fp.Overridden, &orbitv1.ProvenanceOverride{Source: sourceProto(o.Source), Value: o.Value})
		}
		out = append(out, fp)
	}
	return out
}

func sourceProto(s target.Source) *orbitv1.ProvenanceSource {
	layer := map[target.Layer]orbitv1.ProvenanceLayer{
		target.LayerDefault: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_DEFAULT,
		target.LayerTarget:  orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET,
		target.LayerRunFile: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_RUN_FILE,
		target.LayerEnv:     orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV,
		target.LayerFlag:    orbitv1.ProvenanceLayer_PROVENANCE_LAYER_FLAG,
	}[s.Layer]
	detail := s.String()
	return &orbitv1.ProvenanceSource{Layer: layer, Detail: detail}
}
