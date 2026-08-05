package scenario

import (
	"fmt"
	"math"
	"strconv"

	"gopkg.in/yaml.v3"
)

// FleetScenario is the declarative population mode (`kind: fleet`, ADR-0004): a
// generated topology of gNBs and a fleet of UEs running continuous behaviours
// (mobility, traffic) concurrently for a duration. Unlike the step scenario
// (ADR-0003) it is direct-drive, not an API client.
type FleetScenario struct {
	Kind        string      `yaml:"kind"`
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Core        Core        `yaml:"core"`
	Credentials Credentials `yaml:"credentials"`
	Topology    Topology    `yaml:"topology"`
	Fleet       FleetSpec   `yaml:"fleet"`
	Behaviors   Behaviors   `yaml:"behaviors"`
	Run         RunSpec     `yaml:"run"`
}

// Topology generates the gNBs.
type Topology struct {
	GNBs GNBGen `yaml:"gnbs"`
}

// GNBGen generates Count gNBs with IDs from IDBase, each bound to a distinct
// operator-supplied source IP (used for both the N2 SCTP source and N3), laid
// out on a grid for the mobility model.
type GNBGen struct {
	Count     int      `yaml:"count"`
	IDBase    uint32   `yaml:"id_base"`
	SourceIPs []string `yaml:"source_ips"`
	// N3IPs optionally gives each gNB a data-plane address distinct from its
	// N2 source. Empty means N3 rides source_ips, which is right only where
	// one interface carries both — a testbed with separated N2/N3 networks
	// must set it, or the gNB advertises an N3 address the UPF cannot reach.
	N3IPs   []string `yaml:"n3_ips"`
	Layout  string   `yaml:"layout"` // "grid" (default)
	Spacing float64  `yaml:"spacing_m"`
}

// FleetSpec generates the UE population.
type FleetSpec struct {
	Count        int    `yaml:"count"`
	SUPIBase     string `yaml:"supi_base"`
	Distribution string `yaml:"distribution"` // "even" (default)
	AttachRate   string `yaml:"attach_rate"`  // e.g. "10/s"
	PDUSession   bool   `yaml:"pdu_session"`
}

// Behaviors run concurrently for Run.Duration.
type Behaviors struct {
	Mobility *MobilityBehavior `yaml:"mobility"`
	Traffic  *TrafficBehavior  `yaml:"traffic"`
	Latency  *LatencyBehavior  `yaml:"latency"`
}

// LatencyBehavior samples user-plane RTT over the UEs' own N3 data paths for
// the duration of the run. Without it the run reports no user-plane latency at
// all, which is the honest state — zeros would read as an instant data path.
type LatencyBehavior struct {
	Target   string `yaml:"target"`   // IPv4 to echo (no port); required
	Interval string `yaml:"interval"` // between probe rounds, e.g. "1s"
	UEs      int    `yaml:"ues"`      // UEs sampled per round (default 4)
	Timeout  string `yaml:"timeout"`  // per echo, e.g. "1s"
}

type MobilityBehavior struct {
	Model    string `yaml:"model"`    // "random_walk"
	Speed    string `yaml:"speed"`    // e.g. "3m/s"
	Handover string `yaml:"handover"` // "xn" | "n2"
}

type TrafficBehavior struct {
	Mix []TrafficShare `yaml:"mix"`
}

// TrafficShare assigns a fraction of the fleet to either a synthetic traffic
// profile (`profile:` — a loom constant-rate flow) or a REAL application
// cohort (`app:` — members run loom's voip/http/video engines against an N6
// loomd, design §8). Exactly one of Profile/App must be set per entry.
type TrafficShare struct {
	Profile string  `yaml:"profile"` // synthetic: web | video | voip | full-buffer
	App     string  `yaml:"app"`     // app cohort: voip | http | video (real loom app engines)
	Share   float64 `yaml:"share"`
	Rate    string  `yaml:"rate"`
	Target  string  `yaml:"target"`

	// App-cohort fields (ignored on profile entries).
	// StartAfter delays this cohort's start, measured from the beginning of
	// the behaviour phase ("2m"). Empty starts with the run. Staggering
	// cohorts is how you see what ADDING load does: a mix that all begins at
	// once shows the steady state and never the transition.
	StartAfter string            `yaml:"start_after"`
	Name       string            `yaml:"name"`         // cohort label (default: the app name); unique across cohorts
	Peer       string            `yaml:"peer"`         // N6 loomd control address ("host:port"), required
	Token      string            `yaml:"token"`        // loomd bearer token ("" = unauthenticated)
	PeerDataIP string            `yaml:"peer_data_ip"` // N6 media address when it differs from the control host
	Params     map[string]string `yaml:"params"`       // app knobs, passed verbatim (same grammar as `orbit ue app`)
}

// isApp reports whether the entry declares a real-application cohort.
func (m TrafficShare) isApp() bool { return m.App != "" }

// cohortName is the entry's aggregate label (metrics + report).
func (m TrafficShare) cohortName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.App
}

type RunSpec struct {
	Duration string `yaml:"duration"`
}

// PlacedGNB is a generated gNB with its grid position (metres).
type PlacedGNB struct {
	GNB  GNB
	X, Y float64
}

// FleetUE is a generated subscriber: a SUPI bound to a serving gNB, an initial
// position (at its gNB), and an assigned traffic profile ("" if no mix).
type FleetUE struct {
	SUPI     string
	GNBIndex int
	X, Y     float64
	Profile  string
}

// PeekKind reports a scenario file's `kind` ("" / "steps" = the step runner;
// "fleet" = fleet mode), so `orbit run` can dispatch.
func PeekKind(data []byte) (string, error) {
	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(expandEnv(data), &head); err != nil {
		return "", fmt.Errorf("parse scenario: %w", err)
	}
	return head.Kind, nil
}

// ParseFleet decodes a fleet scenario, expanding ${ENV} first.
// ParseFleet parses a fleet scenario, expanding ${ENV} references against the
// process environment — the CLI convention for keeping secrets like Ki/OPc out
// of the file. Use only for input the local operator controls.
func ParseFleet(data []byte) (*FleetScenario, error) {
	return parseFleet(expandEnv(data))
}

// ParseFleetNoEnv parses a fleet scenario WITHOUT ${ENV} expansion. Use it for
// untrusted input — a scenario submitted over the API — where expanding against
// the server's environment would substitute the server's own variables into the
// run and leak their values back to the client (e.g. via a dial error). A
// ${VAR} in such input is left literal.
func ParseFleetNoEnv(data []byte) (*FleetScenario, error) {
	return parseFleet(data)
}

func parseFleet(data []byte) (*FleetScenario, error) {
	var f FleetScenario
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse fleet scenario: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

const defaultSpacingM = 1000.0

func (f *FleetScenario) validate() error {
	if f.Core.AMF == "" {
		return fmt.Errorf("core.amf is required")
	}
	g := f.Topology.GNBs
	if g.Count < 1 {
		return fmt.Errorf("topology.gnbs.count must be >= 1")
	}
	if len(g.SourceIPs) < g.Count {
		return fmt.Errorf("topology.gnbs needs at least %d source_ips (one per gNB for handover), got %d",
			g.Count, len(g.SourceIPs))
	}
	if f.Fleet.Count < 1 {
		return fmt.Errorf("fleet.count must be >= 1")
	}
	if _, err := strconv.ParseUint(f.Fleet.SUPIBase, 10, 64); err != nil {
		return fmt.Errorf("fleet.supi_base %q is not numeric: %w", f.Fleet.SUPIBase, err)
	}
	if t := f.Behaviors.Traffic; t != nil {
		var sum float64
		names := map[string]bool{}
		for _, m := range t.Mix {
			switch {
			case m.Profile != "" && m.App != "":
				return fmt.Errorf("traffic mix entry cannot set both profile (%q, synthetic) and app (%q, real application); pick one", m.Profile, m.App)
			case m.Profile == "" && m.App == "":
				return fmt.Errorf("every traffic mix entry needs a profile (synthetic) or an app (real application cohort)")
			}
			if m.isApp() {
				switch m.App {
				case "voip", "http", "video":
				default:
					return fmt.Errorf("traffic mix app %q is not supported (voip, http, video)", m.App)
				}
				if m.Peer == "" {
					return fmt.Errorf("app cohort %q needs peer: the N6 loomd control address (host:port)", m.cohortName())
				}
				if !f.Fleet.PDUSession {
					return fmt.Errorf("app cohort %q needs fleet.pdu_session: true (application traffic rides the UEs' GTP-U data paths)", m.cohortName())
				}
				if names[m.cohortName()] {
					return fmt.Errorf("app cohort name %q is used twice; cohort names are the aggregate key — set a distinct name: per entry", m.cohortName())
				}
				names[m.cohortName()] = true
			}
			sum += m.Share
		}
		if len(t.Mix) > 0 && (sum < 0.999 || sum > 1.001) {
			return fmt.Errorf("traffic mix shares must sum to 1.0, got %.3f", sum)
		}
	}
	return nil
}

// GenGNBs materialises the topology: Count gNBs on a grid, each with its N2
// source IP and its N3 data-plane address (the same address unless n3_ips
// separates them).
func (f *FleetScenario) GenGNBs() []PlacedGNB {
	g := f.Topology.GNBs
	spacing := g.Spacing
	if spacing <= 0 {
		spacing = defaultSpacingM
	}
	side := int(math.Ceil(math.Sqrt(float64(g.Count))))
	out := make([]PlacedGNB, g.Count)
	for i := 0; i < g.Count; i++ {
		ip := g.SourceIPs[i]
		n3 := ip
		if i < len(g.N3IPs) && g.N3IPs[i] != "" {
			n3 = g.N3IPs[i]
		}
		out[i] = PlacedGNB{
			GNB: GNB{
				ID:   g.IDBase + uint32(i),
				Name: fmt.Sprintf("gnb-%d", i+1),
				N3:   n3,
				Bind: ip + ":0",
			},
			X: float64(i%side) * spacing,
			Y: float64(i/side) * spacing,
		}
	}
	return out
}

// GenFleet materialises the UE population: Count SUPIs from SUPIBase, spread
// across the gNBs (even = round-robin), each starting at its serving gNB and
// assigned a traffic profile from the mix.
func (f *FleetScenario) GenFleet(gnbs []PlacedGNB) []FleetUE {
	base, _ := strconv.ParseUint(f.Fleet.SUPIBase, 10, 64)
	width := len(f.Fleet.SUPIBase)
	profiles := f.profileAssignment()

	out := make([]FleetUE, f.Fleet.Count)
	for i := 0; i < f.Fleet.Count; i++ {
		gi := i % len(gnbs)
		out[i] = FleetUE{
			SUPI:     fmt.Sprintf("%0*d", width, base+uint64(i)),
			GNBIndex: gi,
			X:        gnbs[gi].X,
			Y:        gnbs[gi].Y,
			Profile:  profiles[i],
		}
	}
	return out
}

// mixCounts allocates the fleet across the mix entries proportionally to
// their shares (deterministic contiguous blocks; the last entry absorbs
// rounding) — the population machinery behind both the per-UE profile labels
// and the app-cohort sizes.
func (f *FleetScenario) mixCounts() []int {
	mix := f.Behaviors.Traffic.Mix
	counts := make([]int, len(mix))
	i := 0
	for mi, m := range mix {
		n := int(math.Round(m.Share * float64(f.Fleet.Count)))
		if mi == len(mix)-1 {
			n = f.Fleet.Count - i // last entry absorbs rounding
		}
		if n > f.Fleet.Count-i {
			n = f.Fleet.Count - i
		}
		if n < 0 {
			n = 0
		}
		counts[mi] = n
		i += n
	}
	return counts
}

// profileAssignment returns a per-UE traffic label (the profile, or the
// cohort name for app entries), allocated per mixCounts. Empty if no mix.
func (f *FleetScenario) profileAssignment() []string {
	out := make([]string, f.Fleet.Count)
	if f.Behaviors.Traffic == nil || len(f.Behaviors.Traffic.Mix) == 0 {
		return out
	}
	mix := f.Behaviors.Traffic.Mix
	label := func(m TrafficShare) string {
		if m.isApp() {
			return m.cohortName()
		}
		return m.Profile
	}
	i := 0
	for mi, n := range f.mixCounts() {
		for k := 0; k < n && i < f.Fleet.Count; k++ {
			out[i] = label(mix[mi])
			i++
		}
	}
	for ; i < f.Fleet.Count; i++ {
		out[i] = label(mix[len(mix)-1])
	}
	return out
}

// AppCohort is one derived real-application cohort (mix entries with app:
// set), sized by the same contiguous share allocation that assigns profiles
// to UEs.
type AppCohort struct {
	Name, App               string
	Peer, Token, PeerDataIP string
	Params                  map[string]string
	Count                   int
	StartAfter              string
}

// AppCohorts derives the app-traffic cohorts from the traffic mix. Cohorts
// allocated zero UEs (tiny shares in tiny fleets) are dropped.
func (f *FleetScenario) AppCohorts() []AppCohort {
	if f.Behaviors.Traffic == nil {
		return nil
	}
	mix := f.Behaviors.Traffic.Mix
	counts := f.mixCounts()
	var out []AppCohort
	for i, m := range mix {
		if !m.isApp() || counts[i] == 0 {
			continue
		}
		out = append(out, AppCohort{
			Name: m.cohortName(), App: m.App,
			Peer: m.Peer, Token: m.Token, PeerDataIP: m.PeerDataIP,
			Params: m.Params, Count: counts[i], StartAfter: m.StartAfter,
		})
	}
	return out
}
