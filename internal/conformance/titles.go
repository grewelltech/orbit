package conformance

import (
	"fmt"
	"strings"
)

// The clause-title table (ADR-0011 rule 9): the headings of the clauses the
// checks' primary references cite, and of the procedure and procedure
// family above each, as each specification prints them, per release. A
// check's group is derived from it (GroupOf); no check names its own.
//
// Every title was read from the body heading of the version listed in the
// specification's read list, the latest version of each release from Rel-15
// through LatestRelease, fetched from https://www.3gpp.org/ftp/Specs/archive/.
// A title that differs between releases has one span per wording; a release
// in which the clause does not exist has no span.

// titleLevel is the part a clause plays in a group path.
type titleLevel int

const (
	// levelClause is a cited clause that is neither a family nor a leaf,
	// such as a procedure's "Successful Operation".
	levelClause titleLevel = iota
	// levelFamily is a procedure family ("UE Mobility Management
	// Procedures"), or the clause above a non-procedure leaf.
	levelFamily
	// levelLeaf is a procedure ("Handover Preparation"), or a clause that
	// is not a procedure and names the group itself (erroneous-data
	// handling, NAS security, the requirements of TS 33.501).
	levelLeaf
)

// titleSpan is one wording of a clause's heading and the releases it holds
// in, From through To.
type titleSpan struct {
	From, To Release
	Title    string
}

// clauseTitle is one clause of the table.
type clauseTitle struct {
	Clause string
	Level  titleLevel
	Spans  []titleSpan
}

// specTitles is one specification's part of the table.
type specTitles struct {
	// Spec is the specification as a SpecRef names it ("TS 38.413").
	Spec string
	// Label is the first segment of its group paths: the protocol's
	// abbreviation from the specification's title where it has one
	// ("NGAP"), else the specification number.
	Label string
	// Read lists the version of each release the titles were read in.
	Read    []SpecVersion
	Clauses []clauseTitle
}

// allReleases is one wording held from Rel-15 through LatestRelease.
func allReleases(title string) []titleSpan {
	return []titleSpan{{From: 15, To: LatestRelease, Title: title}}
}

// clauseTitles is the table. Its tests hold every span to the releases its
// read list covers and every primary reference of the built-in checks to an
// entry.
var clauseTitles = []specTitles{
	{
		// "NG-RAN; NG Application Protocol (NGAP)".
		Spec: "TS 38.413", Label: "NGAP",
		Read: []SpecVersion{
			{Release: 15, Version: "V15.13.0", Archive: "38413-fd0.zip"},
			{Release: 16, Version: "V16.18.0", Archive: "38413-gi0.zip"},
			{Release: 17, Version: "V17.15.0", Archive: "38413-hf0.zip"},
			{Release: 18, Version: "V18.11.0", Archive: "38413-ib0.zip"},
			{Release: 19, Version: "V19.4.0", Archive: "38413-j40.zip"},
		},
		Clauses: []clauseTitle{
			{"8.3", levelFamily, allReleases("UE Context Management Procedures")},
			{"8.3.2", levelLeaf, allReleases("UE Context Release Request (NG-RAN node initiated)")},
			{"8.3.2.2", levelClause, allReleases("Successful Operation")},
			{"8.3.3", levelLeaf, allReleases("UE Context Release (AMF initiated)")},
			{"8.3.3.2", levelClause, allReleases("Successful Operation")},
			{"8.4", levelFamily, allReleases("UE Mobility Management Procedures")},
			{"8.4.1", levelLeaf, allReleases("Handover Preparation")},
			{"8.4.1.2", levelClause, allReleases("Successful Operation")},
			{"8.4.4", levelLeaf, allReleases("Path Switch Request")},
			{"8.4.4.2", levelClause, allReleases("Successful Operation")},
			{"8.4.5", levelLeaf, allReleases("Handover Cancellation")},
			{"8.4.5.2", levelClause, allReleases("Successful Operation")},
			{"8.7", levelFamily, allReleases("Interface Management Procedures")},
			{"8.7.1", levelLeaf, allReleases("NG Setup")},
			{"8.7.1.2", levelClause, allReleases("Successful Operation")},
			{"8.7.1.4", levelClause, allReleases("Abnormal Conditions")},
			{"8.7.2", levelLeaf, allReleases("RAN Configuration Update")},
			{"8.7.2.2", levelClause, allReleases("Successful Operation")},
			{"8.7.4", levelLeaf, allReleases("NG Reset")},
			{"8.7.4.2.2", levelClause, allReleases("NG Reset initiated by the NG-RAN node")},
			{"10", levelFamily, allReleases("Handling of Unknown, Unforeseen and Erroneous Protocol Data")},
			{"10.2", levelLeaf, allReleases("Transfer Syntax Error")},
			{"10.3.5", levelLeaf, allReleases("Missing IE or IE group")},
			{"10.6", levelLeaf, allReleases("Handling of AP ID")},
		},
	},
	{
		// "Non-Access-Stratum (NAS) protocol for 5G System (5GS); Stage 3".
		Spec: "TS 24.501", Label: "NAS",
		Read: []SpecVersion{
			{Release: 15, Version: "V15.7.0", Archive: "24501-f70.zip"},
			{Release: 16, Version: "V16.15.0", Archive: "24501-gf0.zip"},
			{Release: 17, Version: "V17.18.1", Archive: "24501-hi1.zip"},
			{Release: 18, Version: "V18.14.0", Archive: "24501-ie0.zip"},
			{Release: 19, Version: "V19.8.0", Archive: "24501-j80.zip"},
		},
		Clauses: []clauseTitle{
			{"4.4", levelFamily, allReleases("NAS security")},
			// The subclauses of 4.4.3 and 4.4.4 are titled "General" and the
			// like, so these two name the group.
			{"4.4.3", levelLeaf, allReleases("Handling of NAS COUNT and NAS sequence number")},
			{"4.4.3.1", levelClause, allReleases("General")},
			{"4.4.3.3", levelClause, allReleases("Integrity protection and verification")},
			{"4.4.4", levelLeaf, allReleases("Integrity protection of NAS signalling messages")},
			{"4.4.4.3", levelClause, allReleases("Integrity checking of NAS signalling messages in the AMF")},
			{"5.4", levelFamily, allReleases("5GMM common procedures")},
			{"5.4.1", levelLeaf, allReleases("Primary authentication and key agreement procedure")},
			{"5.4.1.3.7", levelClause, allReleases("Abnormal cases")},
			{"5.4.2", levelLeaf, allReleases("Security mode control procedure")},
			{"5.4.2.2", levelClause, allReleases("NAS security mode control initiation by the network")},
			{"5.4.2.4", levelClause, allReleases("NAS security mode control completion by the network")},
			{"5.5", levelFamily, allReleases("5GMM specific procedures")},
			{"5.5.1", levelLeaf, allReleases("Registration procedure")},
			{"5.5.1.2.4", levelClause, allReleases("Initial registration accepted by the network")},
			{"5.5.1.2.5", levelClause, allReleases("Initial registration not accepted by the network")},
			{"5.5.1.2.8", levelClause, allReleases("Abnormal cases on the network side")},
			{"5.5.2", levelLeaf, allReleases("De-registration procedure")},
			{"5.5.2.2.2", levelClause, allReleases("UE-initiated de-registration procedure completion")},
			{"5.6", levelFamily, allReleases("5GMM connection management procedures")},
			{"5.6.1", levelLeaf, allReleases("Service request procedure")},
			// 5.6.1.4 has no subclauses in Rel-15.
			{"5.6.1.4.1", levelClause, []titleSpan{{16, 19, "UE is not using 5GS services with control plane CIoT 5GS optimization"}}},
			{"5.6.2", levelLeaf, allReleases("Paging procedure")},
			{"5.6.2.2.1", levelClause, allReleases("General")},
			{"6.4", levelFamily, allReleases("UE-requested 5GSM procedures")},
			{"6.4.1", levelLeaf, allReleases("UE-requested PDU session establishment procedure")},
			{"6.4.1.3", levelClause, allReleases("UE-requested PDU session establishment procedure accepted by the network")},
			// 6.4.1.4 has no subclauses in V15.7.0.
			{"6.4.1.4.1", levelClause, []titleSpan{{16, 19, "General"}}},
			{"6.4.3", levelLeaf, allReleases("UE-requested PDU session release procedure")},
			{"6.4.3.3", levelClause, allReleases("UE-requested PDU session release procedure accepted by the network")},
		},
	},
	{
		// "Security architecture and procedures for 5G system".
		Spec: "TS 33.501", Label: "TS 33.501",
		Read: []SpecVersion{
			{Release: 15, Version: "V15.20.0", Archive: "33501-fk0.zip"},
			{Release: 16, Version: "V16.20.0", Archive: "33501-gk0.zip"},
			{Release: 17, Version: "V17.16.0", Archive: "33501-hg0.zip"},
			{Release: 18, Version: "V18.12.0", Archive: "33501-ic0.zip"},
			{Release: 19, Version: "V19.7.0", Archive: "33501-j70.zip"},
		},
		Clauses: []clauseTitle{
			{"5.5", levelFamily, allReleases("Requirements on the AMF")},
			{"5.5.2", levelLeaf, allReleases("Signalling data integrity")},
			{"6.4", levelFamily, allReleases("NAS security mechanisms")},
			{"6.4.3.2", levelLeaf, allReleases("NAS integrity activation")},
			{"6.7.3", levelFamily, allReleases("Procedures for AS algorithm selection")},
			{"6.7.3.1", levelLeaf, allReleases("Xn-handover")},
		},
	},
	{
		// "System architecture for the 5G System (5GS); Stage 2".
		Spec: "TS 23.501", Label: "TS 23.501",
		Read: []SpecVersion{
			{Release: 15, Version: "V15.13.0", Archive: "23501-fd0.zip"},
			{Release: 16, Version: "V16.20.0", Archive: "23501-gk0.zip"},
			{Release: 17, Version: "V17.15.0", Archive: "23501-hf0.zip"},
			{Release: 18, Version: "V18.12.0", Archive: "23501-ic0.zip"},
			{Release: 19, Version: "V19.9.0", Archive: "23501-j90.zip"},
		},
		Clauses: []clauseTitle{
			{"5.8", levelFamily, allReleases("User Plane Management")},
			{"5.8.2.9", levelLeaf, allReleases(`Functionality of Sending of "End marker"`)},
			{"5.8.2.9.1", levelClause, allReleases(`UPF Constructing the "End marker" Packets`)},
		},
	},
	{
		// "General Packet Radio System (GPRS) Tunnelling Protocol User Plane
		// (GTPv1-U)".
		Spec: "TS 29.281", Label: "GTPv1-U",
		Read: []SpecVersion{
			{Release: 15, Version: "V15.7.0", Archive: "29281-f70.zip"},
			{Release: 16, Version: "V16.2.0", Archive: "29281-g20.zip"},
			{Release: 17, Version: "V17.5.0", Archive: "29281-h50.zip"},
			{Release: 18, Version: "V18.4.0", Archive: "29281-i40.zip"},
			{Release: 19, Version: "V19.2.0", Archive: "29281-j20.zip"},
		},
		Clauses: []clauseTitle{
			{"5.2", levelFamily, allReleases("GTP-U Extension Header")},
			{"5.2.2.7", levelLeaf, allReleases("PDU Session Container")},
			{"7.2", levelFamily, allReleases("Path Management Messages")},
			{"7.2.1", levelLeaf, allReleases("Echo Request")},
			{"7.3", levelFamily, allReleases("Tunnel Management Messages")},
			{"7.3.1", levelLeaf, allReleases("Error Indication")},
		},
	},
}

// specTitlesOf returns the table's part for spec.
func specTitlesOf(spec string) (specTitles, bool) {
	for _, s := range clauseTitles {
		if s.Spec == spec {
			return s, true
		}
	}
	return specTitles{}, false
}

// clause returns the table's entry for a clause number.
func (s specTitles) clause(num string) (clauseTitle, bool) {
	for _, c := range s.Clauses {
		if c.Clause == num {
			return c, true
		}
	}
	return clauseTitle{}, false
}

// in returns the clause's heading in release rel, false when the clause does
// not exist in rel.
func (c clauseTitle) in(rel Release) (string, bool) {
	for _, s := range c.Spans {
		if rel >= s.From && rel <= s.To {
			return s.Title, true
		}
	}
	return "", false
}

// clauseNumber is the clause number a reference's Clause starts with:
// "5.6.1.4.1" of "5.6.1.4.1 (5.6.1.4 in Rel-15) Uplink data status a)".
func clauseNumber(clause string) string {
	f := strings.Fields(clause)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// parentClause is the clause number one level up, "" at the top.
func parentClause(num string) string {
	i := strings.LastIndex(num, ".")
	if i < 0 {
		return ""
	}
	return num[:i]
}

// validateTitles checks the table's own rules: every span lies within the
// releases the specification's titles were read in, spans do not overlap,
// and no title is empty or holds the path separator.
func validateTitles(table []specTitles) error {
	for _, s := range table {
		read := map[Release]bool{}
		for _, v := range s.Read {
			if !archiveRE.MatchString(v.Archive) {
				return fmt.Errorf("%s: read list names %q, want a 3GPP archive file", s.Spec, v.Archive)
			}
			read[v.Release] = true
		}
		if strings.TrimSpace(s.Label) == "" || strings.Contains(s.Label, "/") {
			return fmt.Errorf("%s: label %q is empty or holds a /", s.Spec, s.Label)
		}
		seen := map[string]bool{}
		for _, c := range s.Clauses {
			if seen[c.Clause] {
				return fmt.Errorf("%s %s: listed twice", s.Spec, c.Clause)
			}
			seen[c.Clause] = true
			if len(c.Spans) == 0 {
				return fmt.Errorf("%s %s: no title", s.Spec, c.Clause)
			}
			held := map[Release]bool{}
			for _, sp := range c.Spans {
				if strings.TrimSpace(sp.Title) == "" || strings.Contains(sp.Title, "/") {
					return fmt.Errorf("%s %s: title %q is empty or holds a /", s.Spec, c.Clause, sp.Title)
				}
				if sp.From > sp.To {
					return fmt.Errorf("%s %s: span %s..%s is reversed", s.Spec, c.Clause, sp.From, sp.To)
				}
				for rel := sp.From; rel <= sp.To; rel++ {
					if !read[rel] {
						return fmt.Errorf("%s %s: %s is not in the read list", s.Spec, c.Clause, rel)
					}
					if held[rel] {
						return fmt.Errorf("%s %s: two titles for %s", s.Spec, c.Clause, rel)
					}
					held[rel] = true
				}
			}
		}
	}
	return nil
}
