// Package meas synthesizes 5G measurement-report events (3GPP TS 38.331
// §5.5.4) from per-cell signal quality, so simulated mobility can drive
// handover decisions without a radio.
//
// In a real UE the RRC measurement framework compares serving-cell and
// neighbour-cell measurements (RSRP/RSRQ) against configured event criteria
// and, when a criterion holds for the time-to-trigger, sends a Measurement
// Report. ORBIT has no radio, so the measurement quantities are *synthesized*
// inputs (a mobility model drives RSRP over time); this package implements the
// spec's triggering logic on top of them. An A3 trigger names the neighbour
// that has become better than the serving cell — the target for a handover.
//
// All quantities are in the logarithmic domain: RSRP in dBm, offsets /
// hysteresis / thresholds in dB (TS 38.133). Per-frequency (Ofn/Ofp) and
// per-cell (Ocn/Ocp) offsets are modelled and default to 0 (intra-frequency,
// no cell individual offset).
package meas

import "time"

// Kind identifies a measurement event type (TS 38.331 §5.5.4).
type Kind string

const (
	A3 Kind = "A3" // neighbour becomes offset better than serving (§5.5.4.4)
	A4 Kind = "A4" // neighbour becomes better than a threshold (§5.5.4.5)
	A5 Kind = "A5" // serving worse than thresh1 AND neighbour better than thresh2 (§5.5.4.6)
)

// Offsets holds the frequency/cell offsets applied to a measurement. Zero
// value = intra-frequency, no cell individual offset.
type Offsets struct {
	Freq float64 // Ofn (neighbour) or Ofp (serving): offsetMO
	Cell float64 // Ocn (neighbour) or Ocp (serving): cellIndividualOffset
}

// Event is a configured measurement-report event: its entering/leaving
// criteria and its time-to-trigger.
type Event interface {
	Kind() Kind
	TimeToTrigger() time.Duration
	// entering reports the event's entering criterion given the serving-cell
	// measurement mp and a neighbour measurement mn (both RSRP, dBm).
	entering(mp, mn float64) bool
	// leaving reports the event's leaving criterion.
	leaving(mp, mn float64) bool
}

// EventA3 — neighbour becomes Offset dB better than the serving cell.
// TS 38.331 §5.5.4.4:
//
//	entering (A3-1): Mn + Ofn + Ocn − Hys > Mp + Ofp + Ocp + Off
//	leaving  (A3-2): Mn + Ofn + Ocn + Hys < Mp + Ofp + Ocp + Off
type EventA3 struct {
	Offset     float64 // a3-Offset (dB)
	Hysteresis float64 // hysteresis (dB)
	TTT        time.Duration
	Neighbour  Offsets // Ofn, Ocn
	Serving    Offsets // Ofp, Ocp
}

func (e EventA3) Kind() Kind                   { return A3 }
func (e EventA3) TimeToTrigger() time.Duration { return e.TTT }
func (e EventA3) entering(mp, mn float64) bool {
	return mn+e.Neighbour.Freq+e.Neighbour.Cell-e.Hysteresis >
		mp+e.Serving.Freq+e.Serving.Cell+e.Offset
}
func (e EventA3) leaving(mp, mn float64) bool {
	return mn+e.Neighbour.Freq+e.Neighbour.Cell+e.Hysteresis <
		mp+e.Serving.Freq+e.Serving.Cell+e.Offset
}

// EventA4 — neighbour becomes better than an absolute threshold.
// TS 38.331 §5.5.4.5:
//
//	entering (A4-1): Mn + Ofn + Ocn − Hys > Thresh
//	leaving  (A4-2): Mn + Ofn + Ocn + Hys < Thresh
type EventA4 struct {
	Threshold  float64 // a4-Threshold (dBm)
	Hysteresis float64
	TTT        time.Duration
	Neighbour  Offsets
}

func (e EventA4) Kind() Kind                   { return A4 }
func (e EventA4) TimeToTrigger() time.Duration { return e.TTT }
func (e EventA4) entering(_ float64, mn float64) bool {
	return mn+e.Neighbour.Freq+e.Neighbour.Cell-e.Hysteresis > e.Threshold
}
func (e EventA4) leaving(_ float64, mn float64) bool {
	return mn+e.Neighbour.Freq+e.Neighbour.Cell+e.Hysteresis < e.Threshold
}

// EventA5 — serving becomes worse than Threshold1 AND a neighbour becomes
// better than Threshold2. TS 38.331 §5.5.4.6:
//
//	entering: (A5-1) Mp + Hys < Thresh1  AND  (A5-2) Mn + Ofn + Ocn − Hys > Thresh2
//	leaving:  (A5-3) Mp − Hys > Thresh1  OR   (A5-4) Mn + Ofn + Ocn + Hys < Thresh2
type EventA5 struct {
	Threshold1 float64 // a5-Threshold1: serving worse than this (dBm)
	Threshold2 float64 // a5-Threshold2: neighbour better than this (dBm)
	Hysteresis float64
	TTT        time.Duration
	Neighbour  Offsets
}

func (e EventA5) Kind() Kind                   { return A5 }
func (e EventA5) TimeToTrigger() time.Duration { return e.TTT }
func (e EventA5) entering(mp, mn float64) bool {
	servingBad := mp+e.Hysteresis < e.Threshold1
	neighbourGood := mn+e.Neighbour.Freq+e.Neighbour.Cell-e.Hysteresis > e.Threshold2
	return servingBad && neighbourGood
}
func (e EventA5) leaving(mp, mn float64) bool {
	servingOK := mp-e.Hysteresis > e.Threshold1
	neighbourBad := mn+e.Neighbour.Freq+e.Neighbour.Cell+e.Hysteresis < e.Threshold2
	return servingOK || neighbourBad
}
