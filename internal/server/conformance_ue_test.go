package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
)

const (
	confKi  = "5122250214c33e723a5dd523fc145fc0"
	confOPc = "981d464c7c52eb6e5036234984ad0bcf"
)

// conformanceUE maps the request's UE fields: none is no UE; a SUPI needs
// parseable keys, the request's PLMN and a gNB N3 IP; UE fields without a
// SUPI are refused. Errors never carry the keys.
func TestConformanceUE(t *testing.T) {
	creds := &orbitv1.Credentials{Ki: confKi, Opc: confOPc}
	base := func() *orbitv1.RunConformanceRequest {
		return &orbitv1.RunConformanceRequest{Mcc: "208", Mnc: "93", Supi: "208930100000011", Credentials: creds, GnbN3: "10.0.0.5"}
	}
	with := func(f func(m *orbitv1.RunConformanceRequest)) *orbitv1.RunConformanceRequest {
		m := base()
		f(m)
		return m
	}
	tests := []struct {
		name    string
		req     *orbitv1.RunConformanceRequest
		want    conformance.UESpec
		wantErr string
	}{
		{name: "no UE", req: &orbitv1.RunConformanceRequest{Mcc: "208", Mnc: "93"}},
		{name: "defaults the DNN", req: base(),
			want: conformance.UESpec{SUPI: "208930100000011", Ki: confKi, OPc: confOPc, DNN: "internet", GNBN3: "10.0.0.5"}},
		{name: "full", req: with(func(m *orbitv1.RunConformanceRequest) { m.Dnn, m.Traffic = "ims", "10.1.1.1:7" }),
			want: conformance.UESpec{SUPI: "208930100000011", Ki: confKi, OPc: confOPc, DNN: "ims", GNBN3: "10.0.0.5", Traffic: "10.1.1.1:7"}},
		{name: "no credentials", req: with(func(m *orbitv1.RunConformanceRequest) { m.Credentials = nil }), wantErr: "Ki"},
		{name: "bad OPc", req: with(func(m *orbitv1.RunConformanceRequest) { m.Credentials = &orbitv1.Credentials{Ki: confKi, Opc: "12"} }), wantErr: "OPc"},
		{name: "SUPI outside the PLMN", req: with(func(m *orbitv1.RunConformanceRequest) { m.Supi = "001010000000001" }), wantErr: "mcc+mnc"},
		{name: "no gNB N3", req: with(func(m *orbitv1.RunConformanceRequest) { m.GnbN3 = "" }), wantErr: "gnb_n3"},
		{name: "IPv6 gNB N3", req: with(func(m *orbitv1.RunConformanceRequest) { m.GnbN3 = "0:0:0:0:0:0:0:1" }), wantErr: "IPv6 N3 is not supported yet (#156)"},
		{name: "IPv4-mapped gNB N3 is canonicalised", req: with(func(m *orbitv1.RunConformanceRequest) { m.GnbN3 = "::ffff:10.0.0.5" }),
			want: conformance.UESpec{SUPI: "208930100000011", Ki: confKi, OPc: confOPc, DNN: "internet", GNBN3: "10.0.0.5"}},
		{name: "bad traffic", req: with(func(m *orbitv1.RunConformanceRequest) { m.Traffic = "10.1.1.1" }), wantErr: "traffic"},
		{name: "UE fields without a SUPI", req: with(func(m *orbitv1.RunConformanceRequest) { m.Supi = "" }), wantErr: "need supi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := conformanceUE(tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), confKi) || strings.Contains(err.Error(), confOPc) {
					t.Errorf("error %q carries key material", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("UE = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A bad UE section is refused as InvalidArgument before any check runs.
func TestRunConformanceRejectsBadUE(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "192.0.2.1:38412", Mcc: "208", Mnc: "93", GnbN3: "10.0.0.5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() {
		t.Fatalf("a check ran: %+v", stream.Msg())
	}
	var ce *connect.Error
	if err := stream.Err(); !connectErrorAs(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
