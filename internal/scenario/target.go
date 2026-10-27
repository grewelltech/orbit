package scenario

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
)

// TargetRef is a scenario file's top-level `target:` (ADR-0008 §1): the
// name of a target, or a target document written inline. It is read from
// the file as written, before the scenario's own ${ENV} expansion, so an
// inline target expands its ${VAR}s by the target loader's rules (an unset
// one is an error, not an empty string). The fleet behaviours' own
// `target:` keys (an echo address) are unrelated.
type TargetRef struct {
	Name   string
	Inline []byte // the inline document, as YAML; nil for a name
}

// Set reports whether the file has a top-level target.
func (r TargetRef) Set() bool { return r.Name != "" || r.Inline != nil }

// ReadTargetRef reads a scenario file's top-level target, if any.
func ReadTargetRef(data []byte) (TargetRef, error) {
	var head struct {
		Target yaml.Node `yaml:"target"`
	}
	// A file can be valid YAML only once its ${VAR}s are expanded (a flow
	// mapping holding ${ORBIT_KI}); then the target is read from the
	// expanded text, and an inline one must be readable as written.
	rawErr := yaml.Unmarshal(data, &head)
	if rawErr != nil {
		if err := yaml.Unmarshal(expandEnv(data), &head); err != nil {
			return TargetRef{}, fmt.Errorf("parse scenario: %w", err)
		}
		if head.Target.Kind == yaml.MappingNode {
			return TargetRef{}, fmt.Errorf("the scenario is not valid YAML as written (%v), so its inline target cannot be read before ${VAR} expansion; write ${VAR} values in block style", rawErr)
		}
	}
	n := head.Target
	switch n.Kind {
	case 0:
		return TargetRef{}, nil
	case yaml.ScalarNode:
		if strings.TrimSpace(n.Value) == "" {
			return TargetRef{}, fmt.Errorf("target: is empty; give a target name or a target document")
		}
		return TargetRef{Name: strings.TrimSpace(n.Value)}, nil
	case yaml.MappingNode:
		// An inline target need not repeat the document header.
		for _, kv := range [][2]string{{"version", strconv.Itoa(target.Version)}, {"kind", target.Kind}, {"name", "inline"}} {
			if mapKey(&n, kv[0]) == nil {
				n.Content = append(n.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: kv[0]},
					&yaml.Node{Kind: yaml.ScalarNode, Value: kv[1]})
			}
		}
		b, err := yaml.Marshal(&n)
		if err != nil {
			return TargetRef{}, err
		}
		return TargetRef{Inline: b}, nil
	}
	return TargetRef{}, fmt.Errorf("target: is a target name or a target document")
}

func mapKey(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// ownSetup lists the fields a file sets that a target supplies: its core,
// credentials and addresses. A file with a target sets none of them; a
// file without one is self-contained when it sets any.
func (f *FleetScenario) ownSetup() []string {
	var out []string
	out = append(out, f.Core.set()...)
	out = append(out, f.Credentials.set()...)
	g := f.Topology.GNBs
	if len(g.SourceIPs) > 0 {
		out = append(out, "topology.gnbs.source_ips")
	}
	if len(g.N3IPs) > 0 {
		out = append(out, "topology.gnbs.n3_ips")
	}
	if g.IDBase != 0 {
		out = append(out, "topology.gnbs.id_base")
	}
	if f.Fleet.SUPIBase != "" {
		out = append(out, "fleet.supi_base")
	}
	if t := f.Behaviors.Traffic; t != nil {
		if b := t.Bulk; b != nil {
			out = append(out, peerSet("behaviors.traffic.bulk", b.Peer, b.Token, b.PeerDataIP)...)
		}
		for i, m := range t.Mix {
			out = append(out, peerSet(fmt.Sprintf("behaviors.traffic.mix[%d]", i), m.Peer, m.Token, m.PeerDataIP)...)
		}
	}
	return out
}

func peerSet(prefix, peer, token, dataIP string) []string {
	var out []string
	for _, f := range []struct{ k, v string }{{"peer", peer}, {"token", token}, {"peer_data_ip", dataIP}} {
		if f.v != "" {
			out = append(out, prefix+"."+f.k)
		}
	}
	return out
}

func (c Core) set() []string {
	var out []string
	for _, f := range []struct {
		k   string
		set bool
	}{
		{"core.amf", c.AMF != ""}, {"core.plmn", c.PLMN.MCC != "" || c.PLMN.MNC != ""}, {"core.tac", c.TAC != 0},
		{"core.slice", c.Slice.SST != 0 || c.Slice.SD != ""}, {"core.dnn", c.DNN != ""},
	} {
		if f.set {
			out = append(out, f.k)
		}
	}
	return out
}

func (c Credentials) set() []string {
	var out []string
	if c.Ki != "" {
		out = append(out, "credentials.ki")
	}
	if c.OPc != "" {
		out = append(out, "credentials.opc")
	}
	return out
}

// ownSetup is the step scenario's: core, credentials, each gNB's id, bind
// and n3, and each UE entry's supi or range base.
func (s *Scenario) ownSetup() []string {
	out := append(s.Core.set(), s.Credentials.set()...)
	for i, g := range s.GNBs {
		for _, f := range []struct {
			k   string
			set bool
		}{{"id", g.ID != 0}, {"bind", g.Bind != ""}, {"n3", g.N3 != ""}} {
			if f.set {
				out = append(out, fmt.Sprintf("gnbs[%d].%s", i, f.k))
			}
		}
	}
	for i, u := range s.UEs {
		if u.SUPI != "" {
			out = append(out, fmt.Sprintf("ues[%d].supi", i))
		}
		if u.Range != nil && u.Range.Base != "" {
			out = append(out, fmt.Sprintf("ues[%d].range.base", i))
		}
	}
	return out
}

// SelfContained reports whether a scenario sets its own core, credentials
// or addresses (the fields a target supplies), and which.
func SelfContained(data []byte) ([]string, error) {
	kind, err := PeekKind(data)
	if err != nil {
		return nil, err
	}
	if kind == "fleet" {
		f, err := decodeFleet(expandEnv(data))
		if err != nil {
			return nil, err
		}
		return f.ownSetup(), nil
	}
	s, err := decodeSteps(expandEnv(data))
	if err != nil {
		return nil, err
	}
	return s.ownSetup(), nil
}

// conflict is the error for a file that sets what its target supplies.
func conflict(own []string, targetName string) error {
	shown := own
	if len(shown) > 3 {
		shown = append(append([]string(nil), shown[:3]...), "…")
	}
	return fmt.Errorf("the scenario sets %s and uses target %s: a file uses a target or its own core/credentials/addresses, not both",
		strings.Join(shown, ", "), targetName)
}

// firstPool is the target's first subscriber pool, which a scenario's UEs
// and keys come from.
func firstPool(t target.Target) (target.Pool, error) {
	if len(t.Subscribers) == 0 || t.Subscribers[0].FirstSUPI == "" {
		return target.Pool{}, fmt.Errorf("target %s has no subscriber pool with a first_supi", t.Name)
	}
	return t.Subscribers[0], nil
}

// applyCore sets the core and credentials from t.
func applyCore(c *Core, cr *Credentials, t target.Target, p target.Pool) {
	c.AMF = t.Core.AMF
	c.PLMN = PLMN{MCC: t.Core.PLMN.MCC, MNC: t.Core.PLMN.MNC}
	c.TAC = t.Core.TAC
	if len(t.Core.Slices) > 0 {
		c.Slice = Slice{SST: t.Core.Slices[0].SST, SD: t.Core.Slices[0].SD}
	}
	c.DNN = t.Core.DNN
	cr.Ki, cr.OPc = p.Ki, p.OPc
}

// addresses expands pool to n addresses for n gNBs: one per gNB, or the
// pool's one address shared when it has only one. Too few is an error
// naming both counts.
func addresses(field string, pool []string, n int) ([]string, error) {
	addrs, err := target.ExpandPool(pool, n)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	switch {
	case len(addrs) == n:
		return addrs, nil
	case len(addrs) == 1:
		out := make([]string, n)
		for i := range out {
			out[i] = addrs[0]
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s has %d addresses, the scenario needs %d (one per gNB)", field, len(addrs), n)
}

// supiAt is the SUPI off places after first, kept to first's width.
func supiAt(first string, off uint64) (string, error) {
	digits, err := ue.NormalizeSUPI(first)
	if err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return "", fmt.Errorf("first_supi %q is not numeric", first)
	}
	return fmt.Sprintf("%0*d", len(digits), n+off), nil
}

// checkPoolSize is the error when the pool holds fewer subscribers than
// the scenario's UEs (a pool with no count is not limited).
func checkPoolSize(p target.Pool, need int) error {
	if p.Count != 0 && uint64(need) > uint64(p.Count) {
		return fmt.Errorf("subscribers[%s] has %d subscribers, the scenario needs %d", p.Name, p.Count, need)
	}
	return nil
}

// ApplyTarget fills a fleet scenario's core, credentials and addresses from
// t (ADR-0008 §1): the core and its first slice, the first pool's keys and
// first SUPI, the gNB ID range's first ID, one N2 and N3 address per gNB
// from the pools, and, for traffic and latency left without one, the first
// responder. A scenario that sets any of these itself is an error.
func (f *FleetScenario) ApplyTarget(t target.Target) error {
	if own := f.ownSetup(); len(own) > 0 {
		return conflict(own, t.Name)
	}
	p, err := firstPool(t)
	if err != nil {
		return err
	}
	if err := checkPoolSize(p, f.Fleet.Count); err != nil {
		return err
	}
	applyCore(&f.Core, &f.Credentials, t, p)
	f.Fleet.SUPIBase = p.FirstSUPI
	g := &f.Topology.GNBs
	g.IDBase = t.RAN.GNBIDs.First
	if g.IDBase == 0 {
		g.IDBase = target.DefaultGNBID
	}
	n := g.Count
	if n < 1 {
		n = 1
	}
	if len(t.RAN.N2SourceIPs) == 0 {
		return fmt.Errorf("target %s has no ran.n2_source_ips", t.Name)
	}
	if g.SourceIPs, err = addresses("ran.n2_source_ips", t.RAN.N2SourceIPs, n); err != nil {
		return err
	}
	for i, a := range g.SourceIPs {
		g.SourceIPs[i] = strings.TrimSuffix(a, ":0")
	}
	if len(t.RAN.N3IPs) > 0 {
		if g.N3IPs, err = addresses("ran.n3_ips", t.RAN.N3IPs, n); err != nil {
			return err
		}
	}
	responder := func(what string) (*target.Responder, error) {
		if len(t.App.Responders) == 0 {
			return nil, fmt.Errorf("%s needs an N6 responder: target %s has no app.responders", what, t.Name)
		}
		return &t.App.Responders[0], nil
	}
	if tr := f.Behaviors.Traffic; tr != nil {
		if b := tr.Bulk; b != nil {
			r, err := responder("behaviors.traffic.bulk")
			if err != nil {
				return err
			}
			b.Peer, b.Token, b.PeerDataIP = r.Control, r.Token, r.DataIP
		}
		for i := range tr.Mix {
			if !tr.Mix[i].isApp() {
				continue
			}
			r, err := responder(fmt.Sprintf("behaviors.traffic.mix[%d] (app %s)", i, tr.Mix[i].App))
			if err != nil {
				return err
			}
			tr.Mix[i].Peer, tr.Mix[i].Token, tr.Mix[i].PeerDataIP = r.Control, r.Token, r.DataIP
		}
	}
	if l := f.Behaviors.Latency; l != nil && l.Target == "" {
		r, err := responder("behaviors.latency")
		if err != nil {
			return err
		}
		if r.DataIP == "" {
			return fmt.Errorf("behaviors.latency needs target: (the address to echo), or app.responders[%s].data_ip in target %s", r.Name, t.Name)
		}
		l.Target = r.DataIP
	}
	return nil
}

// ApplyTarget fills a step scenario from t: the core and its first slice,
// the first pool's keys, each gNB's ID (the range's first, then +1 …),
// N2 bind and N3 address from the pools, and the UEs' SUPIs, allocated in
// file order from the pool's first SUPI so no two entries overlap. A
// scenario that sets any of these itself is an error.
func (s *Scenario) ApplyTarget(t target.Target) error {
	if own := s.ownSetup(); len(own) > 0 {
		return conflict(own, t.Name)
	}
	p, err := firstPool(t)
	if err != nil {
		return err
	}
	applyCore(&s.Core, &s.Credentials, t, p)
	s.targetName = t.Name
	if len(t.App.Responders) > 0 {
		s.echo = t.App.Responders[0].DataIP
	}
	first := t.RAN.GNBIDs.First
	if first == 0 {
		first = target.DefaultGNBID
	}
	n := len(s.GNBs)
	var binds, n3s []string
	if n > 0 && len(t.RAN.N2SourceIPs) > 0 {
		if binds, err = addresses("ran.n2_source_ips", t.RAN.N2SourceIPs, n); err != nil {
			return err
		}
	}
	if n > 0 && len(t.RAN.N3IPs) > 0 {
		if n3s, err = addresses("ran.n3_ips", t.RAN.N3IPs, n); err != nil {
			return err
		}
	}
	for i := range s.GNBs {
		s.GNBs[i].ID = first + uint32(i)
		if binds != nil {
			s.GNBs[i].Bind = binds[i]
		}
		if n3s != nil {
			host := n3s[i]
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			s.GNBs[i].N3 = host
		}
	}
	var off uint64
	for i := range s.UEs {
		u := &s.UEs[i]
		count := 1
		if u.Range != nil {
			count = u.Range.Count
		}
		base, err := supiAt(p.FirstSUPI, off)
		if err != nil {
			return fmt.Errorf("subscribers[%s]: %w", p.Name, err)
		}
		if u.Range != nil {
			u.Range.Base = base
		} else {
			u.SUPI = base
		}
		off += uint64(count)
	}
	return checkPoolSize(p, int(off))
}
