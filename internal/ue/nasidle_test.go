package ue

import (
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

var testGUTI = []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07}

func decodePlain(t *testing.T, b []byte) *f5nas.Message {
	t.Helper()
	m := f5nas.NewMessage()
	buf := append([]byte(nil), b...)
	if err := m.PlainNasDecode(&buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// The complete Service Request carries every IE; the container form
// carries the cleartext IEs and the container only. Both decode.
func TestBuildServiceRequest(t *testing.T) {
	tests := []struct {
		name          string
		p             ServiceRequestParams
		wantContainer bool
	}{
		{"signalling", ServiceRequestParams{GUTI: testGUTI, NgKSI: 3, ServiceType: ServiceTypeSignalling}, false},
		{"data for session 1", ServiceRequestParams{GUTI: testGUTI, NgKSI: 1, ServiceType: ServiceTypeData, UplinkDataStatus: 1 << 1, PDUSessionStatus: 1<<1 | 1<<9}, true},
		{"paging answer with status only", ServiceRequestParams{GUTI: testGUTI, NgKSI: 0, ServiceType: ServiceTypeMobileTerminated, PDUSessionStatus: 1 << 5}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.HasNonCleartext(); got != tc.wantContainer {
				t.Fatalf("HasNonCleartext = %v, want %v", got, tc.wantContainer)
			}
			full, err := BuildServiceRequest(tc.p)
			if err != nil {
				t.Fatal(err)
			}
			req := decodePlain(t, full).ServiceRequest
			if req == nil {
				t.Fatal("not a Service Request")
			}
			if req.GetServiceTypeValue() != tc.p.ServiceType || req.GetNasKeySetIdentifiler() != tc.p.NgKSI {
				t.Errorf("service type %d ngKSI %d, want %d and %d", req.GetServiceTypeValue(), req.GetNasKeySetIdentifiler(), tc.p.ServiceType, tc.p.NgKSI)
			}
			if req.TMSI5GS.Octet != [7]uint8{0xf4, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07} {
				t.Errorf("5G-S-TMSI octets % x", req.TMSI5GS.Octet)
			}
			var ul, st uint16
			if req.UplinkDataStatus != nil {
				ul = bitmap16(req.UplinkDataStatus.Buffer)
			}
			if req.PDUSessionStatus != nil {
				st = bitmap16(req.PDUSessionStatus.Buffer)
			}
			if ul != tc.p.UplinkDataStatus || st != tc.p.PDUSessionStatus {
				t.Errorf("uplink data status %#x, PDU session status %#x; want %#x and %#x", ul, st, tc.p.UplinkDataStatus, tc.p.PDUSessionStatus)
			}

			outer, err := BuildServiceRequestWithContainer(tc.p, full)
			if err != nil {
				t.Fatal(err)
			}
			o := decodePlain(t, outer).ServiceRequest
			if o.UplinkDataStatus != nil || o.PDUSessionStatus != nil {
				t.Error("the container form carries non-cleartext IEs in the clear")
			}
			if o.NASMessageContainer == nil || string(o.NASMessageContainer.Buffer) != string(full) {
				t.Error("the container does not hold the complete request")
			}
		})
	}
	if _, err := BuildServiceRequest(ServiceRequestParams{GUTI: testGUTI[:5]}); err == nil {
		t.Error("a Service Request without a 5G-GUTI built")
	}
	if _, err := BuildServiceRequestWithContainer(ServiceRequestParams{}, nil); err == nil {
		t.Error("a container Service Request without a 5G-GUTI built")
	}
}

// ParseServiceAccept and ParseServiceReject read their IEs and refuse
// other messages.
func TestParseServiceAnswers(t *testing.T) {
	accept := func(fill func(*nasMessage.ServiceAccept)) *f5nas.Message {
		m, gmm := newGmm(f5nas.MsgTypeServiceAccept)
		acc := nasMessage.NewServiceAccept(f5nas.MsgTypeServiceAccept)
		fill(acc)
		gmm.ServiceAccept = acc
		return m
	}
	tests := []struct {
		name string
		fill func(*nasMessage.ServiceAccept)
		want ServiceAccept
	}{
		{"bare", func(*nasMessage.ServiceAccept) {}, ServiceAccept{}},
		{"status and result", func(a *nasMessage.ServiceAccept) {
			a.PDUSessionStatus = &nasType.PDUSessionStatus{Len: 2, Buffer: []uint8{0x02, 0x01}}
			a.PDUSessionReactivationResult = &nasType.PDUSessionReactivationResult{Len: 2, Buffer: []uint8{0x02, 0x00}}
		}, ServiceAccept{PDUSessionStatus: 0x0102, HasPDUSessionStatus: true, ReactivationResult: 0x0002, HasReactivationResult: true}},
		{"one-octet bitmap", func(a *nasMessage.ServiceAccept) {
			a.PDUSessionStatus = &nasType.PDUSessionStatus{Len: 1, Buffer: []uint8{0x20}}
		}, ServiceAccept{PDUSessionStatus: 0x20, HasPDUSessionStatus: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseServiceAccept(accept(tc.fill))
			if err != nil {
				t.Fatal(err)
			}
			if got.PDUSessionStatus != tc.want.PDUSessionStatus || got.HasPDUSessionStatus != tc.want.HasPDUSessionStatus ||
				got.ReactivationResult != tc.want.ReactivationResult || got.HasReactivationResult != tc.want.HasReactivationResult {
				t.Errorf("ParseServiceAccept = %+v, want %+v", got, tc.want)
			}
		})
	}
	withErrors := accept(func(a *nasMessage.ServiceAccept) {
		a.PDUSessionReactivationResultErrorCause = &nasType.PDUSessionReactivationResultErrorCause{Len: 2, Buffer: []uint8{1, 92}}
	})
	if got, err := ParseServiceAccept(withErrors); err != nil || string(got.ReactivationErrors) != string([]byte{1, 92}) {
		t.Errorf("reactivation errors = %v, %v", got, err)
	}

	m, gmm := newGmm(f5nas.MsgTypeServiceReject)
	rej := nasMessage.NewServiceReject(f5nas.MsgTypeServiceReject)
	rej.Cause5GMM.Octet = 22
	gmm.ServiceReject = rej
	if cause, err := ParseServiceReject(m); err != nil || cause != 22 {
		t.Errorf("ParseServiceReject = %d, %v; want 22", cause, err)
	}
	if _, err := ParseServiceAccept(m); err == nil {
		t.Error("a Service Reject parsed as an Accept")
	}
	if _, err := ParseServiceReject(withErrors); err == nil {
		t.Error("a Service Accept parsed as a Reject")
	}
	if _, err := ParseServiceAccept(nil); err == nil {
		t.Error("nil parsed as an Accept")
	}
	if _, err := ParseRegistrationInfo(m); err == nil {
		t.Error("a Service Reject parsed as a Registration Accept")
	}
}

// GPRS timer 3 decodes every unit, with unit 7 as deactivated.
func TestGPRSTimer3(t *testing.T) {
	tests := []struct {
		octet       uint8
		want        time.Duration
		deactivated bool
	}{
		{0x00 | 6, time.Hour, false},         // 6 x 10 minutes
		{0x20 | 2, 2 * time.Hour, false},     // 2 x 1 hour
		{0x40 | 1, 10 * time.Hour, false},    // 1 x 10 hours
		{0x60 | 15, 30 * time.Second, false}, // 15 x 2 seconds
		{0x80 | 3, 90 * time.Second, false},  // 3 x 30 seconds
		{0xa0 | 31, 31 * time.Minute, false}, // 31 x 1 minute
		{0xc0 | 1, 320 * time.Hour, false},   // 1 x 320 hours
		{0xe0 | 5, 0, true},                  // deactivated
		{0x00, 0, false},                     // zero value
		{0x20 | 0x1f, 31 * time.Hour, false}, // the largest value field
	}
	for _, tc := range tests {
		if got, off := gprsTimer3(tc.octet); got != tc.want || off != tc.deactivated {
			t.Errorf("gprsTimer3(%#02x) = %v, %v; want %v, %v", tc.octet, got, off, tc.want, tc.deactivated)
		}
	}
}

// ParseRegistrationInfo reports T3512, a MICO indication and the area
// lists of a Registration Accept, and their absence.
func TestParseRegistrationInfo(t *testing.T) {
	accept := func(fill func(*nasMessage.RegistrationAccept)) *f5nas.Message {
		m, gmm := newGmm(f5nas.MsgTypeRegistrationAccept)
		acc := nasMessage.NewRegistrationAccept(f5nas.MsgTypeRegistrationAccept)
		fill(acc)
		gmm.RegistrationAccept = acc
		return m
	}
	got, err := ParseRegistrationInfo(accept(func(*nasMessage.RegistrationAccept) {}))
	if err != nil || got.HasT3512 || got.MICO || got.ServiceAreaList != nil || got.TAIList != nil {
		t.Errorf("bare Accept = %+v, %v; want nothing set", got, err)
	}
	got, err = ParseRegistrationInfo(accept(func(a *nasMessage.RegistrationAccept) {
		a.T3512Value = &nasType.T3512Value{Len: 1, Octet: 0x00 | 9} // 9 x 10 minutes
		a.MICOIndication = &nasType.MICOIndication{Octet: 0xb1}
		a.ServiceAreaList = &nasType.ServiceAreaList{Len: 2, Buffer: []uint8{1, 2}}
		a.TAIList = &nasType.TAIList{Len: 3, Buffer: []uint8{3, 4, 5}}
	}))
	if err != nil || !got.HasT3512 || got.T3512 != 90*time.Minute || got.T3512Deactivated || !got.MICO ||
		string(got.ServiceAreaList) != "\x01\x02" || string(got.TAIList) != "\x03\x04\x05" {
		t.Errorf("full Accept = %+v, %v", got, err)
	}
	got, _ = ParseRegistrationInfo(accept(func(a *nasMessage.RegistrationAccept) {
		a.T3512Value = &nasType.T3512Value{Len: 1, Octet: 0xe0}
	}))
	if !got.HasT3512 || !got.T3512Deactivated {
		t.Errorf("deactivated T3512 = %+v", got)
	}
}
