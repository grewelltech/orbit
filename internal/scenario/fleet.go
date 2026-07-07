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
	Layout    string   `yaml:"layout"` // "grid" (default)
	Spacing   float64  `yaml:"spacing_m"`
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
}

type MobilityBehavior struct {
	Model    string `yaml:"model"`    // "random_walk"
	Speed    string `yaml:"speed"`    // e.g. "3m/s"
	Handover string `yaml:"handover"` // "xn" | "n2"
}

type TrafficBehavior struct {
	Mix []TrafficShare `yaml:"mix"`
}

// TrafficShare assigns a fraction of the fleet to a named traffic profile.
type TrafficShare struct {
	Profile string  `yaml:"profile"` // web | video | voip | full-buffer
	Share   float64 `yaml:"share"`
	Rate    string  `yaml:"rate"`
	Target  string  `yaml:"target"`
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
func ParseFleet(data []byte) (*FleetScenario, error) {
	var f FleetScenario
	if err := yaml.Unmarshal(expandEnv(data), &f); err != nil {
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
		for _, m := range t.Mix {
			if m.Profile == "" {
				return fmt.Errorf("every traffic mix entry needs a profile")
			}
			sum += m.Share
		}
		if len(t.Mix) > 0 && (sum < 0.999 || sum > 1.001) {
			return fmt.Errorf("traffic mix shares must sum to 1.0, got %.3f", sum)
		}
	}
	return nil
}

// GenGNBs materialises the topology: Count gNBs on a grid, each with a distinct
// source IP (used as both bind and N3).
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
		out[i] = PlacedGNB{
			GNB: GNB{
				ID:   g.IDBase + uint32(i),
				Name: fmt.Sprintf("gnb-%d", i+1),
				N3:   ip,
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

// profileAssignment returns a per-UE traffic profile, allocated proportionally
// to the mix shares (deterministic: contiguous blocks). Empty strings if no mix.
func (f *FleetScenario) profileAssignment() []string {
	out := make([]string, f.Fleet.Count)
	if f.Behaviors.Traffic == nil || len(f.Behaviors.Traffic.Mix) == 0 {
		return out
	}
	mix := f.Behaviors.Traffic.Mix
	i := 0
	for mi, m := range mix {
		n := int(math.Round(m.Share * float64(f.Fleet.Count)))
		if mi == len(mix)-1 {
			n = f.Fleet.Count - i // last profile absorbs rounding
		}
		for k := 0; k < n && i < f.Fleet.Count; k++ {
			out[i] = m.Profile
			i++
		}
	}
	for ; i < f.Fleet.Count; i++ {
		out[i] = mix[len(mix)-1].Profile
	}
	return out
}
