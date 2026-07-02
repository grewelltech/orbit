package gnb

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// atb01Config mirrors the live ATB-01 test target so the fixture bytes are
// exactly what the Phase-0 smoke test sends at the real AMF.
func atb01Config() Config {
	return Config{
		ID:     1,
		Name:   "orbit-gnb-1",
		MCC:    "208",
		MNC:    "93",
		TAC:    1,
		Slices: []SNSSAI{{SST: 1, SD: "010203"}},
	}
}

// goldenNGSetupRequest freezes the encoded NG Setup Request for the ATB-01
// config (deterministic message, §5f of the design). Regenerate consciously:
// any diff means the wire bytes changed and must be re-verified against the
// live AMF before updating.
const goldenNGSetupRequest = "00150039000004001b00080002f839100000010052400d05006f726269742d676e622d310066001000000000010002f839000010080102030015400140"

func TestBuildNGSetupRequestRoundTrip(t *testing.T) {
	pdu, err := BuildNGSetupRequest(atb01Config())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}

	dec, err := ngap.Decode(b)
	if err != nil {
		t.Fatalf("decode of our own encoding failed: %v", err)
	}
	if dec.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		t.Fatalf("decoded present = %d, want initiating message", dec.Present)
	}
	im := dec.InitiatingMessage
	if im.ProcedureCode.Value != ngapType.ProcedureCodeNGSetup {
		t.Fatalf("procedure code = %d, want NGSetup (%d)", im.ProcedureCode.Value, ngapType.ProcedureCodeNGSetup)
	}
	req := im.Value.NGSetupRequest
	if req == nil {
		t.Fatal("decoded NGSetupRequest is nil")
	}

	// IE inventory: ids, criticalities, and the key values.
	var sawRANNode, sawName, sawTAList, sawDRX bool
	for _, ie := range req.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDGlobalRANNodeID:
			sawRANNode = true
			if ie.Criticality.Value != ngapType.CriticalityPresentReject {
				t.Error("GlobalRANNodeID criticality must be reject (TS 38.413 §9.2.6.1)")
			}
			g := ie.Value.GlobalRANNodeID.GlobalGNBID
			if !bytes.Equal(g.PLMNIdentity.Value, []byte{0x02, 0xF8, 0x39}) {
				t.Errorf("PLMN = %x, want 02f839", g.PLMNIdentity.Value)
			}
			id := g.GNBID.GNBID
			if id.BitLength != 24 || !bytes.Equal(id.Bytes, []byte{0x00, 0x00, 0x01}) {
				t.Errorf("gNB ID = %x/%d bits, want 000001/24", id.Bytes, id.BitLength)
			}
		case ngapType.ProtocolIEIDRANNodeName:
			sawName = true
			if ie.Value.RANNodeName.Value != "orbit-gnb-1" {
				t.Errorf("RANNodeName = %q", ie.Value.RANNodeName.Value)
			}
		case ngapType.ProtocolIEIDSupportedTAList:
			sawTAList = true
			if ie.Criticality.Value != ngapType.CriticalityPresentReject {
				t.Error("SupportedTAList criticality must be reject (TS 38.413 §9.2.6.1)")
			}
			items := ie.Value.SupportedTAList.List
			if len(items) != 1 {
				t.Fatalf("supported TA items = %d, want 1", len(items))
			}
			if !bytes.Equal(items[0].TAC.Value, []byte{0x00, 0x00, 0x01}) {
				t.Errorf("TAC = %x, want 000001", items[0].TAC.Value)
			}
			slices := items[0].BroadcastPLMNList.List[0].TAISliceSupportList.List
			if len(slices) != 1 {
				t.Fatalf("slice support items = %d, want 1", len(slices))
			}
			if !bytes.Equal(slices[0].SNSSAI.SST.Value, []byte{0x01}) {
				t.Errorf("SST = %x, want 01", slices[0].SNSSAI.SST.Value)
			}
			if slices[0].SNSSAI.SD == nil || !bytes.Equal(slices[0].SNSSAI.SD.Value, []byte{0x01, 0x02, 0x03}) {
				t.Error("SD missing or != 010203")
			}
		case ngapType.ProtocolIEIDDefaultPagingDRX:
			sawDRX = true
		}
	}
	if !sawRANNode || !sawName || !sawTAList || !sawDRX {
		t.Errorf("missing IEs: ranNode=%v name=%v taList=%v drx=%v", sawRANNode, sawName, sawTAList, sawDRX)
	}

	// Golden bytes (regression guard for deterministic messages, §5f).
	if goldenNGSetupRequest == "GOLDEN_PLACEHOLDER" {
		t.Logf("golden fixture not yet frozen; current encoding: %s", hex.EncodeToString(b))
	} else if got := hex.EncodeToString(b); got != goldenNGSetupRequest {
		t.Errorf("encoded NG Setup Request drifted from golden fixture:\n got  %s\n want %s", got, goldenNGSetupRequest)
	}
}

func TestBuildNGSetupRequestValidation(t *testing.T) {
	bad := []Config{
		{ID: 1, MCC: "208", MNC: "93", TAC: 1},                                                     // no slices
		{ID: 1, MCC: "208", MNC: "93", TAC: 1 << 24, Slices: []SNSSAI{{SST: 1}}},                   // TAC overflow
		{ID: 1 << 24, MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1}}},                   // ID overflow for 24 bits
		{ID: 1, IDBits: 21, MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1}}},             // bits < 22
		{ID: 1, MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1, SD: "01020"}}},            // odd-length SD
		{ID: 1, MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1, SD: "0102030405060708"}}}, // SD too long
	}
	for i, cfg := range bad {
		if _, err := BuildNGSetupRequest(cfg); err == nil {
			t.Errorf("config %d: expected validation error", i)
		}
	}
}

func TestGNBIDBitString(t *testing.T) {
	// 24-bit reference from gnbsim: 0x454647 → bytes 45 46 47, 24 bits.
	bs := gnbIDBitString(0x454647, 24)
	if bs.BitLength != 24 || !bytes.Equal(bs.Bytes, []byte{0x45, 0x46, 0x47}) {
		t.Errorf("24-bit: got %x/%d", bs.Bytes, bs.BitLength)
	}
	// 22-bit ID must be left-aligned in 3 bytes: value 1 → 00 00 04.
	bs = gnbIDBitString(1, 22)
	if bs.BitLength != 22 || !bytes.Equal(bs.Bytes, []byte{0x00, 0x00, 0x04}) {
		t.Errorf("22-bit: got %x/%d", bs.Bytes, bs.BitLength)
	}
	// 32-bit uses 4 bytes, no shift.
	bs = gnbIDBitString(0xDEADBEEF, 32)
	if bs.BitLength != 32 || !bytes.Equal(bs.Bytes, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Errorf("32-bit: got %x/%d", bs.Bytes, bs.BitLength)
	}
}
