package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// gnbN3Arg canonicalises an IPv4-mapped address, passes "" through, and
// refuses IPv6 as InvalidArgument naming the field and #156.
func TestGNBN3Arg(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "10.0.0.5", want: "10.0.0.5"},
		{in: "::ffff:10.0.0.5", want: "10.0.0.5"},
		{in: "0:0:0:0:0:0:0:1", wantErr: true},
		{in: "not-an-ip", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := gnbN3Arg(tc.in)
			if tc.wantErr {
				if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "gnb_n3_addr") {
					t.Fatalf("gnbN3Arg(%q) = %q, %v; want InvalidArgument naming gnb_n3_addr", tc.in, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("gnbN3Arg(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

// Every RPC carrying a gNB N3 address refuses an IPv6 one as InvalidArgument
// before anything is dialled, instead of panicking in PDU session setup.
func TestRPCsRefuseIPv6GNBN3(t *testing.T) {
	const v6 = "0:0:0:0:0:0:0:1"
	uc := newUEClient(t)
	rc, _ := newRunRPCClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := &orbitv1.GnbConfig{Id: 2, IdBits: 24, Name: "g", Mcc: "208", Mnc: "93", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1}}}
	creds := &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"}
	tests := []struct {
		name string
		call func() error
	}{
		{"Register", func() error {
			_, err := uc.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{AmfAddress: "127.0.0.1:1", Supi: "208930100007500",
				Gnb: g, Credentials: creds, PduSession: &orbitv1.PDUSession{PduSessionId: 1, Sst: 1}, GnbN3Addr: v6}))
			return err
		}},
		{"Handover", func() error {
			_, err := uc.Handover(ctx, connect.NewRequest(&orbitv1.HandoverRequest{Supi: "208930100007500", AmfAddress: "127.0.0.1:1",
				TargetGnb: g, GnbN3Addr: v6}))
			return err
		}},
		{"XnHandover", func() error {
			_, err := uc.XnHandover(ctx, connect.NewRequest(&orbitv1.HandoverRequest{Supi: "208930100007500", AmfAddress: "127.0.0.1:1",
				TargetGnb: g, GnbN3Addr: v6}))
			return err
		}},
		{"StartRun", func() error {
			spec := failFastLoadSpec()
			spec.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: 1}
			spec.GnbN3Addr = v6
			_, err := rc.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Load{Load: spec}})) //nolint:staticcheck // the deprecated arm, kept for API clients
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "IPv6 N3 is not supported yet (#156)") {
				t.Errorf("%s with an IPv6 gNB N3: %v, want InvalidArgument citing #156", tc.name, err)
			}
		})
	}
}
