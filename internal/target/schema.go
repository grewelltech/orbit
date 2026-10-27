// Package target implements ADR-0008's targets: where a test runs (the
// core, the subscriber pool, the RAN address pools and the N6 responders),
// kept on the client, loaded from YAML, and layered with a run file, the
// environment and flags into one resolved configuration whose every field
// records the layer that set it (provenance).
//
// The package resolves; it does not run anything. The CLI and the server's
// Resolve call share it so there is one implementation of the precedence
// rules (ADR-0008 §3).
package target

import (
	"errors"
	"fmt"
)

// Version is the target schema version this package reads.
const Version = 1

// Kind is the value of a target document's kind key.
const Kind = "target"

// Target is a v1 target document (ADR-0008 §1). Beyond §1's example it
// carries subscribers[].routing_indicator, ran.gnb_name and core.upf_n3, so
// every existing CLI flag has a field to map to.
type Target struct {
	Version     int    `yaml:"version" json:"version"`
	Kind        string `yaml:"kind" json:"kind"`
	Name        string `yaml:"name" json:"name"`
	Core        Core   `yaml:"core" json:"core"`
	Subscribers []Pool `yaml:"subscribers,omitempty" json:"subscribers,omitempty"`
	RAN         RAN    `yaml:"ran" json:"ran"`
	App         App    `yaml:"app" json:"app"`
}

// Core is the core network under test.
type Core struct {
	// AMF is the AMF's N2 endpoint, host:port.
	AMF string `yaml:"amf,omitempty" json:"amf,omitempty"`
	// PLMN is kept as strings so leading zeros survive.
	PLMN PLMN   `yaml:"plmn" json:"plmn"`
	TAC  uint32 `yaml:"tac,omitempty" json:"tac,omitempty"`
	// Slices are the served S-NSSAIs; the first is the default for PDU
	// sessions.
	Slices []Slice `yaml:"slices,omitempty" json:"slices,omitempty"`
	DNN    string  `yaml:"dnn,omitempty" json:"dnn,omitempty"`
	// Profile names an internal/coreprofile profile ("" is the server's
	// default).
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
	// UPFN3 is the UPF's N3 endpoint, host:port, for checks that reach it
	// directly.
	UPFN3 string `yaml:"upf_n3,omitempty" json:"upf_n3,omitempty"`
}

// PLMN is a PLMN identity.
type PLMN struct {
	MCC string `yaml:"mcc,omitempty" json:"mcc,omitempty"`
	MNC string `yaml:"mnc,omitempty" json:"mnc,omitempty"`
}

// Slice is an S-NSSAI.
type Slice struct {
	SST uint32 `yaml:"sst" json:"sst"`
	SD  string `yaml:"sd,omitempty" json:"sd,omitempty"`
}

// Pool is a named block of test subscribers.
type Pool struct {
	Name      string `yaml:"name" json:"name"`
	FirstSUPI string `yaml:"first_supi,omitempty" json:"first_supi,omitempty"`
	Count     uint32 `yaml:"count,omitempty" json:"count,omitempty"`
	// Ki and OPc are secret: provenance, logs and archives mask them.
	Ki  string `yaml:"ki,omitempty" json:"ki,omitempty"`
	OPc string `yaml:"opc,omitempty" json:"opc,omitempty"`
	// RoutingIndicator is the SUCI routing indicator ("" is "0").
	RoutingIndicator string `yaml:"routing_indicator,omitempty" json:"routing_indicator,omitempty"`
}

// RAN is the RAN side's addressing. Every address belongs to Node.
type RAN struct {
	// Node is reserved for the orbit node ADR: only "" (the server's own
	// host) is accepted in v1.
	Node string `yaml:"node,omitempty" json:"node,omitempty"`
	// GNBName prefixes gNB names; a per-gNB index is appended.
	GNBName string `yaml:"gnb_name,omitempty" json:"gnb_name,omitempty"`
	// N2SourceIPs are the N2 (SCTP) source addresses runs lease from. Each
	// entry is an IP, an ip:port (port 0 picks an ephemeral port; any other
	// port is an exact lease), an IP range (a-b) or a CIDR; ranges and
	// CIDRs take no port.
	N2SourceIPs []string `yaml:"n2_source_ips,omitempty" json:"n2_source_ips,omitempty"`
	// N3IPs are the gNBs' N3 (GTP-U) addresses, a pool in the same forms as
	// N2SourceIPs: an IP, ip:port or host name, or an IP-only range or CIDR.
	N3IPs  []string `yaml:"n3_ips,omitempty" json:"n3_ips,omitempty"`
	GNBIDs GNBIDs   `yaml:"gnb_ids" json:"gnb_ids"`
}

// GNBIDs is the gNB ID range runs lease from; zero values take the
// server's default range.
type GNBIDs struct {
	First uint32 `yaml:"first,omitempty" json:"first,omitempty"`
	Count uint32 `yaml:"count,omitempty" json:"count,omitempty"`
	Bits  uint32 `yaml:"bits,omitempty" json:"bits,omitempty"`
}

// App is the application side: the N6 responders workloads use by name.
type App struct {
	Responders []Responder `yaml:"responders,omitempty" json:"responders,omitempty"`
}

// Responder is an N6 responder: one registered by join (ADR-0009), named
// only, or a static one with its control address, token and data IP.
type Responder struct {
	Name    string `yaml:"name" json:"name"`
	Control string `yaml:"control,omitempty" json:"control,omitempty"`
	// Token is secret.
	Token  string `yaml:"token,omitempty" json:"token,omitempty"`
	DataIP string `yaml:"data_ip,omitempty" json:"data_ip,omitempty"`
}

// ErrNodeReserved refuses a ran.node other than "": addresses on another
// host belong to the orbit node ADR, which fills the field in.
var ErrNodeReserved = errors.New(`ran.node is reserved: v1 accepts only "" (the server's own host); other nodes come with the orbit node ADR`)

// Validate checks what the schema itself requires: the version and kind, a
// name, the reserved ran.node, and unique names in the named lists. Field
// values (a PLMN's digits, an address's form) are checked where they are
// used.
func (t *Target) Validate() error {
	if t.Version != Version {
		return fmt.Errorf("target %q: version %d, want %d", t.Name, t.Version, Version)
	}
	if t.Kind != Kind {
		return fmt.Errorf("target %q: kind %q, want %q", t.Name, t.Kind, Kind)
	}
	if t.Name == "" {
		return errors.New("target: name is required")
	}
	if t.RAN.Node != "" {
		return fmt.Errorf("target %q: %w (got %q)", t.Name, ErrNodeReserved, t.RAN.Node)
	}
	seen := map[string]bool{}
	for _, p := range t.Subscribers {
		if p.Name == "" || seen[p.Name] {
			return fmt.Errorf("target %q: subscriber pools need distinct names (got %q)", t.Name, p.Name)
		}
		seen[p.Name] = true
	}
	seen = map[string]bool{}
	for _, r := range t.App.Responders {
		if r.Name == "" || seen[r.Name] {
			return fmt.Errorf("target %q: responders need distinct names (got %q)", t.Name, r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}
