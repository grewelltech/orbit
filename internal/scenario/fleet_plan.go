package scenario

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// ErrNotTyped marks a fleet scenario field whose text the typed plan form
// cannot carry exactly (a rate or duration it would rewrite, a share that
// is both a profile and an app). The caller keeps the scenario as text.
var ErrNotTyped = errors.New("not expressible in the typed form")

// Proto is the behaviours in their typed plan form (ADR-0008), the one
// planBehaviors turns back into this scenario text. Durations become
// milliseconds and rates bits per second; a value that would not come back
// as the same text is ErrNotTyped.
func (b Behaviors) Proto() (*orbitv1.Behaviors, error) {
	out := &orbitv1.Behaviors{}
	if m := b.Mobility; m != nil {
		pm := &orbitv1.Mobility{Model: m.Model}
		if m.Speed != "" {
			v, err := strconv.ParseFloat(strings.TrimSuffix(m.Speed, "m/s"), 64)
			if err != nil || strconv.FormatFloat(v, 'f', -1, 64)+"m/s" != m.Speed {
				return nil, fmt.Errorf("behaviors.mobility.speed %q: %w", m.Speed, ErrNotTyped)
			}
			pm.SpeedMps = v
		}
		switch m.Handover {
		case "":
		case "xn":
			pm.Handover = orbitv1.HandoverKind_HANDOVER_KIND_XN
		case "n2":
			pm.Handover = orbitv1.HandoverKind_HANDOVER_KIND_N2
		default:
			return nil, fmt.Errorf("behaviors.mobility.handover %q: %w", m.Handover, ErrNotTyped)
		}
		out.Mobility = pm
	}
	if t := b.Traffic; t != nil {
		pt := &orbitv1.Traffic{}
		for _, s := range t.Mix {
			ps := &orbitv1.TrafficShare{Name: s.Name, Share: s.Share}
			switch {
			case s.Profile != "" && s.App != "":
				return nil, fmt.Errorf("traffic share %q is both a profile and an app: %w", s.cohortName(), ErrNotTyped)
			case s.Profile != "":
				if s.Peer != "" || s.Token != "" || s.PeerDataIP != "" || len(s.Params) > 0 || s.StartAfter != "" {
					return nil, fmt.Errorf("traffic share %q carries app fields: %w", s.Profile, ErrNotTyped)
				}
				bps, err := typedBitRate(s.Rate)
				if err != nil {
					return nil, fmt.Errorf("traffic share %q rate: %w", s.Profile, err)
				}
				ps.Kind = &orbitv1.TrafficShare_Synthetic{Synthetic: &orbitv1.SyntheticProfile{Profile: s.Profile, RateBps: bps, Target: s.Target}}
			case s.App != "":
				if s.Rate != "" || s.Target != "" {
					return nil, fmt.Errorf("traffic cohort %q carries profile fields: %w", s.cohortName(), ErrNotTyped)
				}
				after, err := typedMs(s.StartAfter)
				if err != nil {
					return nil, fmt.Errorf("traffic cohort %q start_after: %w", s.cohortName(), err)
				}
				ps.Kind = &orbitv1.TrafficShare_App{App: &orbitv1.AppCohort{App: s.App, StartAfterMs: after,
					Peer: s.Peer, Token: s.Token, PeerDataIp: s.PeerDataIP, Params: s.Params}}
			default:
				return nil, fmt.Errorf("a traffic share with neither a profile nor an app: %w", ErrNotTyped)
			}
			pt.Mix = append(pt.Mix, ps)
		}
		if k := t.Bulk; k != nil {
			up, err := typedBitRate(k.UplinkRate)
			if err != nil {
				return nil, fmt.Errorf("bulk uplink_rate: %w", err)
			}
			down, err := typedBitRate(k.DownlinkRate)
			if err != nil {
				return nil, fmt.Errorf("bulk downlink_rate: %w", err)
			}
			if k.UEs < 0 || k.PacketSize < 0 {
				return nil, fmt.Errorf("bulk ues or packet_size below zero: %w", ErrNotTyped)
			}
			pt.Bulk = &orbitv1.BulkTraffic{Ues: uint32(k.UEs), UplinkBps: up, DownlinkBps: down,
				Peer: k.Peer, Token: k.Token, PeerDataIp: k.PeerDataIP, PacketSize: uint32(k.PacketSize)}
		}
		out.Traffic = pt
	}
	if l := b.Latency; l != nil {
		iv, err := typedMs(l.Interval)
		if err != nil {
			return nil, fmt.Errorf("behaviors.latency.interval: %w", err)
		}
		to, err := typedMs(l.Timeout)
		if err != nil {
			return nil, fmt.Errorf("behaviors.latency.timeout: %w", err)
		}
		if l.UEs < 0 {
			return nil, fmt.Errorf("behaviors.latency.ues below zero: %w", ErrNotTyped)
		}
		out.Latency = &orbitv1.LatencyProbe{Target: l.Target, IntervalMs: iv, Ues: uint32(l.UEs), TimeoutMs: to}
	}
	return out, nil
}

// typedMs is a duration's text as whole milliseconds ("" is 0). The typed
// form gives it back as "<n>ms", which parses to the same duration.
func typedMs(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d%time.Millisecond != 0 {
		return 0, fmt.Errorf("%q: %w", s, ErrNotTyped)
	}
	return uint64(d / time.Millisecond), nil
}

// bitRateUnits are the units the typed form writes a bit rate with, largest
// first, as planBehaviors does.
var bitRateUnits = []struct {
	suffix string
	scale  uint64
}{{"Gbps", 1_000_000_000}, {"Mbps", 1_000_000}, {"Kbps", 1_000}, {"bps", 1}}

// typedBitRate is a bit rate's text ("8Mbps") as bits per second ("" is 0),
// when the typed form writes it back the same: a whole number in the largest
// unit that divides it.
func typedBitRate(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	for _, u := range bitRateUnits {
		num, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(num, 10, 64)
		if err != nil || n == 0 || n > (1<<63)/u.scale {
			break
		}
		bps := n * u.scale
		for _, w := range bitRateUnits {
			if bps%w.scale == 0 {
				if w.suffix != u.suffix {
					break // "1000Kbps" comes back as "1Mbps"
				}
				return bps, nil
			}
		}
		break
	}
	return 0, fmt.Errorf("%q: %w", s, ErrNotTyped)
}
