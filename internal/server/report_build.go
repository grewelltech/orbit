package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/report"
)

// Chart line colours. Fixed rather than themed: a report is a document that
// outlives the palette it was generated under, and a saved file whose colours
// shift with a later release is not the same artefact.
const (
	colDown = "#0b6a78"
	colUp   = "#c2621a"
	colP50  = "#0b6a78"
	colP90  = "#7a5ea8"
	colP99  = "#a11c2c"
)

// buildReport turns an archived run into report data.
//
// Built from the ARCHIVE rather than from live state, which is what makes a
// report reproducible: the same run renders the same report tomorrow, and can
// be regenerated later with a better template. That only became possible once
// runs were retained (#80) and persisted (#86); before then a report had to be
// baked at run end or not exist.
func buildReport(a *orbitv1.RunArchive, version, host string) report.Data {
	run := a.GetRun()
	started := time.Unix(0, run.GetStartedUnixNano())
	var ended time.Time
	if run.GetEndedUnixNano() != 0 {
		ended = time.Unix(0, run.GetEndedUnixNano())
	}
	var dur time.Duration
	if !ended.IsZero() {
		dur = ended.Sub(started)
	}

	d := report.Data{
		RunID:      run.GetRunId(),
		Name:       run.GetName(),
		Kind:       strings.TrimPrefix(run.GetKind().String(), "RUN_KIND_"),
		State:      runStateLabel(run.GetState()),
		Started:    started,
		Ended:      ended,
		Duration:   dur,
		Err:        run.GetError(),
		Version:    version,
		SourceHost: host,
		Generated:  time.Now(),
	}
	d.Duration = dur

	switch {
	case a.GetFleetProgress() != nil:
		buildFleetSections(&d, a)
	case a.GetLoadProgress() != nil:
		buildLoadSections(&d, a)
	}

	addCharts(&d, a.GetFrames())
	addEvents(&d, a)
	addConfig(&d, a)
	return d
}

func buildFleetSections(d *report.Data, a *orbitv1.RunArchive) {
	fp := a.GetFleetProgress()
	rep := a.GetFleetReport()

	attached, failed := fp.GetAttached(), fp.GetAttachFailed()
	if rep != nil {
		attached, failed = rep.GetAttached(), rep.GetAttachFailed()
	}
	d.Headline = append(d.Headline,
		report.KV{Label: "UEs attached", Value: fmt.Sprint(attached),
			Detail: fmt.Sprintf("%d failed", failed)},
		report.KV{Label: "app sessions", Value: fmt.Sprint(fp.GetAppSessions()),
			Detail: fmt.Sprintf("%d traffic flows", fp.GetTrafficFlows())},
		report.KV{Label: "downlink", Value: humanBytes(fp.GetDownlinkBytes()),
			Detail: fmt.Sprintf("%s packets", humanCount(fp.GetDownlinkPackets()))},
		report.KV{Label: "uplink", Value: humanBytes(fp.GetUplinkBytes()),
			Detail: fmt.Sprintf("%s packets", humanCount(fp.GetUplinkPackets()))},
	)
	if h := fp.GetHandovers(); h > 0 || fp.GetHandoverErrors() > 0 {
		d.Headline = append(d.Headline, report.KV{
			Label: "handovers", Value: fmt.Sprint(h),
			Detail: fmt.Sprintf("%d failed", fp.GetHandoverErrors()),
		})
	}

	addProcedureTable(d, fp.GetLatency())

	if up := fp.GetUpLatency(); up != nil && up.GetCount() > 0 {
		d.Tables = append(d.Tables, report.Table{
			Title:   "User-plane round trip",
			Columns: []string{"probes", "lost", "p50 ms", "p90 ms", "p99 ms", "max ms"},
			Rows: [][]string{{
				humanCount(fp.GetUpProbes()), humanCount(fp.GetUpProbesLost()),
				ms(up.GetP50Ms()), ms(up.GetP90Ms()), ms(up.GetP99Ms()), ms(up.GetMaxMs()),
			}},
			Note: "ICMP echoes over the UEs' own N3 data paths — the tunnelled path, not a control-plane measurement.",
		})
	}

	if cohorts := fp.GetCohorts(); len(cohorts) > 0 {
		addCohortTables(d, cohorts)
	}

	if gnbs := fp.GetPerGnb(); len(gnbs) > 1 {
		rows := make([][]string, 0, len(gnbs))
		for _, g := range gnbs {
			rows = append(rows, []string{g.GetGnb(), fmt.Sprint(g.GetSucceeded()), fmt.Sprint(g.GetFailed())})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
		d.Tables = append(d.Tables, report.Table{
			Title: "Population by gNB", Columns: []string{"gNB", "attached", "failed"}, Rows: rows,
		})
	}
}

func buildLoadSections(d *report.Data, a *orbitv1.RunArchive) {
	lp := a.GetLoadProgress()
	d.Headline = append(d.Headline,
		report.KV{Label: "attempted", Value: fmt.Sprint(lp.GetAttempted())},
		report.KV{Label: "succeeded", Value: fmt.Sprint(lp.GetSucceeded()),
			Detail: fmt.Sprintf("%d failed", lp.GetFailed())},
		report.KV{Label: "achieved rate", Value: fmt.Sprintf("%.2f/s", lp.GetAchievedRate())},
	)
	addProcedureTable(d, lp.GetLatency())
}

// addProcedureTable renders control-plane procedure latency, with the caveat
// that makes the numbers readable — attach-family procedures are measured once
// per UE during the attach phase and describe that burst, not the whole run.
func addProcedureTable(d *report.Data, rows []*orbitv1.ProcedureLatency) {
	if len(rows) == 0 {
		return
	}
	out := make([][]string, 0, len(rows))
	attachPhase := false
	for _, r := range rows {
		switch r.GetProcedure() {
		case "attach", "registration", "pdu_session":
			attachPhase = true
		}
		out = append(out, []string{
			r.GetProcedure(), humanCount(r.GetCount()),
			ms(r.GetP50Ms()), ms(r.GetP90Ms()), ms(r.GetP99Ms()), ms(r.GetMaxMs()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	t := report.Table{
		Title:   "Control-plane procedures",
		Columns: []string{"procedure", "samples", "p50 ms", "p90 ms", "p99 ms", "max ms"},
		Rows:    out,
	}
	if attachPhase {
		t.Note = "attach, registration and pdu_session are sampled once per UE during the attach " +
			"phase, so they describe the opening burst rather than the whole run. Handover figures, " +
			"where present, span the run."
		d.Notes = append(d.Notes,
			"Attach-family latencies come from the attach phase only — they are not a measure of "+
				"control-plane health for the duration of the run.")
	}
	d.Tables = append(d.Tables, t)
}

func addCohortTables(d *report.Data, cohorts []*orbitv1.CohortProgress) {
	rows := make([][]string, 0, len(cohorts))
	for _, c := range cohorts {
		rows = append(rows, []string{
			c.GetName(), c.GetApp(), fmt.Sprint(c.GetUes()), fmt.Sprint(c.GetFailed()),
			cohortQuality(c), farEndSummary(c.GetFarEnd()),
		})
	}
	d.Tables = append(d.Tables, report.Table{
		Title:   "Application cohorts",
		Columns: []string{"cohort", "app", "UEs", "failed", "quality (p5/p50/p95)", "N6 far end"},
		Rows:    rows,
		Note: "Quality is the across-member distribution over the last sampling interval. " +
			"The far end is an independent observer beyond the UPF; a cohort with no far end " +
			"says so rather than reporting zero.",
	})
}

// cohortQuality names the metrics that apply to the cohort's app. Families that
// do not apply are omitted rather than reported as zero — a voip cohort has no
// TTFB, and printing 0 would claim it was measured.
func cohortQuality(c *orbitv1.CohortProgress) string {
	var parts []string
	add := func(label string, q *orbitv1.Quantiles, unit string) {
		if q == nil {
			return
		}
		parts = append(parts, fmt.Sprintf("%s %s/%s/%s%s",
			label, trim(q.GetP5()), trim(q.GetP50()), trim(q.GetP95()), unit))
	}
	switch c.GetApp() {
	case "voip":
		add("MOS", c.GetMos(), "")
	case "http":
		add("TTFB", c.GetTtfbMs(), "ms")
		add("goodput", c.GetGoodputMbps(), "Mbps")
	case "video":
		add("bitrate", c.GetBitrateKbps(), "kbps")
		add("stall", c.GetStallTimeMs(), "ms")
		add("startup", c.GetStartupMs(), "ms")
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

func farEndSummary(f *orbitv1.FarEndView) string {
	if f == nil {
		return "—"
	}
	if !f.GetAvailable() {
		if r := f.GetReason(); r != "" {
			return r
		}
		return "not observed"
	}
	s := fmt.Sprintf("%s, %s", mbps(f.GetBitsPerSec()), humanBytes(f.GetBytes()))
	if r := f.GetRequests(); r > 0 {
		s += fmt.Sprintf(", %s requests", humanCount(r))
	}
	return s
}

// addCharts plots the retained series. Only fleet runs carry user-plane
// counters, so a load run gets the latency chart alone rather than a pair of
// flat-zero throughput lines implying nothing moved.
func addCharts(d *report.Data, frames []*orbitv1.TelemetryFrame) {
	if len(frames) < 2 {
		return
	}
	var dl, ul, att, p50, p90, p99 []float64
	for _, f := range frames {
		if fp := f.GetFleet(); fp != nil {
			dl = append(dl, fp.GetDownlinkBps())
			ul = append(ul, fp.GetUplinkBps())
			att = append(att, float64(fp.GetAttached()))
			if l := pickLatency(fp.GetLatency()); l != nil {
				p50 = append(p50, l.GetP50Ms())
				p90 = append(p90, l.GetP90Ms())
				p99 = append(p99, l.GetP99Ms())
			}
		} else if lp := f.GetLoad(); lp != nil {
			att = append(att, float64(lp.GetSucceeded()))
			if l := pickLatency(lp.GetLatency()); l != nil {
				p50 = append(p50, l.GetP50Ms())
				p90 = append(p90, l.GetP90Ms())
				p99 = append(p99, l.GetP99Ms())
			}
		}
	}

	if anyNonZero(dl) || anyNonZero(ul) {
		d.Charts = append(d.Charts, report.Chart{
			Title: "Throughput", Unit: "bit/s",
			Series: []report.Series{
				{Name: "downlink", Color: colDown, Values: dl},
				{Name: "uplink", Color: colUp, Values: ul},
			},
		})
	}
	if anyNonZero(att) {
		d.Charts = append(d.Charts, report.Chart{
			Title:  "Attached UEs",
			Series: []report.Series{{Name: "attached", Color: colDown, Values: att}},
			Height: 110,
		})
	}
	if anyNonZero(p99) {
		d.Charts = append(d.Charts, report.Chart{
			Title: "Control-plane latency", Unit: "ms",
			Series: []report.Series{
				{Name: "p50", Color: colP50, Values: p50},
				{Name: "p90", Color: colP90, Values: p90},
				{Name: "p99", Color: colP99, Values: p99},
			},
			Height: 120,
		})
	}
}

// pickLatency mirrors the dashboard's choice so the report and the live view
// agree about which procedure the latency chart is showing.
func pickLatency(rows []*orbitv1.ProcedureLatency) *orbitv1.ProcedureLatency {
	byName := func(n string) *orbitv1.ProcedureLatency {
		for _, r := range rows {
			if r.GetProcedure() == n {
				return r
			}
		}
		return nil
	}
	for _, n := range []string{"handover_xn", "handover_n2", "attach", "registration"} {
		if r := byName(n); r != nil {
			return r
		}
	}
	if len(rows) > 0 {
		return rows[0]
	}
	return nil
}

func addEvents(d *report.Data, a *orbitv1.RunArchive) {
	evs := a.GetEvents()
	if len(evs) == 0 {
		return
	}
	for _, e := range evs {
		d.Events = append(d.Events, report.Event{
			At:       time.Unix(0, e.GetUnixNano()).Format("15:04:05.000"),
			Severity: strings.ToLower(strings.TrimPrefix(e.GetSeverity().String(), "EVENT_SEVERITY_")),
			Kind:     strings.ToLower(e.GetKind()),
			Subject:  e.GetSupi(),
			Message:  e.GetMessage(),
		})
	}
	if n := a.GetEventsDroppedBefore(); n > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf(
			"%d earlier events were evicted from the run's ring buffer and are not in this report.", n))
	}
}

func addConfig(d *report.Data, a *orbitv1.RunArchive) {
	switch spec := a.GetSpec().(type) {
	case *orbitv1.RunArchive_FleetSpec:
		if y := spec.FleetSpec.GetScenarioYaml(); y != "" {
			d.ConfigLabel = "Scenario"
			d.Config = y
		}
	case *orbitv1.RunArchive_LoadSpec:
		d.ConfigLabel = "Load specification"
		d.Config = loadSpecSummary(spec.LoadSpec)
	}
	if d.Config == "" {
		d.Notes = append(d.Notes,
			"The configuration for this run was not recorded — it predates configuration capture, "+
				"so this report cannot be compared against another on its settings.")
	}
}

// loadSpecSummary renders a load spec as YAML-ish text. Hand-rolled rather than
// marshalled so the field order is the order someone would read them in, and so
// nothing new is exposed by accident when the proto grows a field.
func loadSpecSummary(s *orbitv1.LoadRunSpec) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "amf: %s\n", s.GetAmfAddress())
	fmt.Fprintf(&b, "base_imsi: %s\n", s.GetBaseImsi())
	fmt.Fprintf(&b, "count: %d\n", s.GetCount())
	if r := s.GetRate(); r > 0 {
		fmt.Fprintf(&b, "rate: %g/s\n", r)
	}
	if c := s.GetConcurrency(); c > 0 {
		fmt.Fprintf(&b, "concurrency: %d\n", c)
	}
	for _, g := range s.GetGnbs() {
		fmt.Fprintf(&b, "gnb:\n  id: %d\n  name: %s\n  plmn: %s-%s\n  tac: %d\n",
			g.GetId(), g.GetName(), g.GetMcc(), g.GetMnc(), g.GetTac())
		for _, sl := range g.GetSlices() {
			fmt.Fprintf(&b, "  slice: sst=%d sd=%s\n", sl.GetSst(), sl.GetSd())
		}
	}
	return b.String()
}

func anyNonZero(v []float64) bool {
	for _, x := range v {
		if x != 0 {
			return true
		}
	}
	return false
}

func humanBytes(b uint64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(u), 0
	for n := b / u; n >= u && exp < 4; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

func humanCount(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func ms(v float64) string { return trim(v) }
func mbps(bps float64) string {
	return fmt.Sprintf("%.2f Mbps", bps/1e6)
}

// trim formats a float without trailing noise — a report table reads better
// with 6.4 than with 6.400000000000001.
func trim(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
