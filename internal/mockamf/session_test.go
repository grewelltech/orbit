package mockamf

import (
	"strings"
	"testing"
)

// A UPF needs the IPv4 address the mock advertises as its uplink endpoint.
func TestNewRequiresUPFAddressWithUPF(t *testing.T) {
	base := Config{
		Ki: make([]byte, 16), OPc: make([]byte, 16), SQN: make([]byte, 6), AMF: make([]byte, 2),
		MCC: "208", MNC: "93", SST: 1,
	}
	tests := []struct {
		name    string
		upf     UPF
		addr    string
		wantErr string
	}{
		{name: "no UPF, no address", upf: nil, addr: ""},
		{name: "UPF with IPv4", upf: nopUPF{}, addr: "127.0.0.10"},
		{name: "UPF without address", upf: nopUPF{}, addr: "", wantErr: "UPFAddress"},
		{name: "UPF with IPv6", upf: nopUPF{}, addr: "::1", wantErr: "IPv4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.UPF, cfg.UPFAddress = tc.upf, tc.addr
			_, err := New(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// The setup request and the path switch acknowledge encode, and the tunnels
// they carry read back.
func TestSessionMessagesEncode(t *testing.T) {
	setup, err := buildPDUSessionResourceSetupRequest(7, 3, 1, []byte{0x7e, 0x02}, Tunnel{Addr: "127.0.0.10", TEID: 0x10001}, 1, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encode(setup); err != nil {
		t.Fatalf("encode setup request: %v", err)
	}
	ul := Tunnel{Addr: "127.0.0.10", TEID: 0x10002}
	ack, err := buildPathSwitchRequestAcknowledge(7, 4, []switchedItem{{id: 1, newUL: &ul}, {id: 2}}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encode(ack); err != nil {
		t.Fatalf("encode path switch ack: %v", err)
	}
	got, err := tunnelFrom(gtpTunnel(ul))
	if err != nil || got != ul {
		t.Errorf("tunnel round trip = %+v (%v), want %+v", got, err, ul)
	}
	acc, err := buildPDUSessionEstablishmentAccept(1, 0, []byte{10, 60, 0, 1}, 1, []byte{1, 2, 3}, "internet")
	if err != nil || len(acc) == 0 {
		t.Fatalf("encode Establishment Accept: %v", err)
	}
}

type nopUPF struct{}

func (nopUPF) SessionUp(SessionInfo)   {}
func (nopUPF) PathSwitched(PathSwitch) {}
