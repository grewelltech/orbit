package gnb

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// The encoded RAN CONFIGURATION UPDATE for each IE set: procedure 35,
// criticality reject, then exactly the IEs asked for, in order. The RAN
// Node Name and Supported TA List values are byte-for-byte those of the NG
// SETUP REQUEST golden, with the RAN CONFIGURATION UPDATE criticalities
// (ignore, reject).
func TestBuildRANConfigurationUpdate(t *testing.T) {
	const (
		name   = "0052400d05006f726269742d676e622d31"
		taList = "0066001000000000010002f8390000100801020300"
	)
	tests := []struct {
		name    string
		opts    []RANConfigUpdateOption
		want    string
		wantIEs []int64
	}{
		{name: "no IE", want: "00230003000000"},
		{name: "RAN Node Name only", opts: []RANConfigUpdateOption{WithRANNodeName("orbit-gnb-1")},
			want: "00230014000001" + name, wantIEs: []int64{ngapType.ProtocolIEIDRANNodeName}},
		{name: "Supported TA List only", opts: []RANConfigUpdateOption{WithSupportedTAList(atb01Config())},
			want: "00230017000001" + strings.TrimSuffix(taList, "00"), wantIEs: []int64{ngapType.ProtocolIEIDSupportedTAList}},
		{name: "both, in the order given", opts: []RANConfigUpdateOption{WithSupportedTAList(atb01Config()), WithRANNodeName("orbit-gnb-1")},
			want:    "00230028000002" + strings.TrimSuffix(taList, "00") + name,
			wantIEs: []int64{ngapType.ProtocolIEIDSupportedTAList, ngapType.ProtocolIEIDRANNodeName}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pdu, err := BuildRANConfigurationUpdate(tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ngap.Encode(pdu)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(b); got != tc.want {
				t.Errorf("encoded = %s, want %s", got, tc.want)
			}
			dec, err := ngap.Decode(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			im := dec.InitiatingMessage
			if im == nil || im.ProcedureCode.Value != ngapType.ProcedureCodeRANConfigurationUpdate ||
				im.Criticality.Value != ngapType.CriticalityPresentReject || im.Value.RANConfigurationUpdate == nil {
				t.Fatalf("decoded PDU is not a RAN CONFIGURATION UPDATE with criticality reject: %+v", dec)
			}
			var ids []int64
			for _, ie := range im.Value.RANConfigurationUpdate.ProtocolIEs.List {
				ids = append(ids, ie.Id.Value)
				want := ngapType.CriticalityPresentIgnore
				if ie.Id.Value == ngapType.ProtocolIEIDSupportedTAList {
					want = ngapType.CriticalityPresentReject
				}
				if ie.Criticality.Value != want {
					t.Errorf("IE %d criticality = %d, want %d (TS 38.413 9.2.6.4)", ie.Id.Value, ie.Criticality.Value, want)
				}
			}
			if len(ids) != len(tc.wantIEs) {
				t.Fatalf("IEs = %v, want %v", ids, tc.wantIEs)
			}
			for i := range ids {
				if ids[i] != tc.wantIEs[i] {
					t.Errorf("IEs = %v, want %v", ids, tc.wantIEs)
				}
			}
		})
	}
}

// An option that cannot build its IE fails the whole message.
func TestBuildRANConfigurationUpdateErrors(t *testing.T) {
	bad := atb01Config()
	bad.Slices = nil
	badPLMN := atb01Config()
	badPLMN.MCC = "20"
	tests := []struct {
		name    string
		opt     RANConfigUpdateOption
		wantErr string
	}{
		{name: "empty name", opt: WithRANNodeName(""), wantErr: "RAN node name is empty"},
		{name: "config without a slice", opt: WithSupportedTAList(bad), wantErr: "at least one supported S-NSSAI"},
		{name: "bad PLMN", opt: WithSupportedTAList(badPLMN), wantErr: "RAN configuration update:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pdu, err := BuildRANConfigurationUpdate(WithRANNodeName("ok"), tc.opt)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if pdu.InitiatingMessage != nil {
				t.Error("a failed build returned a partial PDU")
			}
		})
	}
}

// scriptTransport is a Transport that records what is written and answers
// reads from a script; after the script, reads block until Close.
type scriptTransport struct {
	writeErr error
	replies  chan scriptReply
	wrote    chan []byte
	closed   chan struct{}
}

type scriptReply struct {
	payload []byte
	err     error
}

func newScriptTransport(replies ...scriptReply) *scriptTransport {
	s := &scriptTransport{replies: make(chan scriptReply, len(replies)), wrote: make(chan []byte, 4), closed: make(chan struct{})}
	for _, r := range replies {
		s.replies <- r
	}
	return s
}

func (s *scriptTransport) WriteNGAP(stream uint16, pdu []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.wrote <- append([]byte{byte(stream)}, pdu...)
	return nil
}

func (s *scriptTransport) ReadMsg([]byte) ([]byte, uint16, uint32, error) {
	select {
	case r := <-s.replies:
		return r.payload, 0, 60, r.err
	case <-s.closed:
		return nil, 0, 0, io.EOF
	}
}

func (s *scriptTransport) Close() error { close(s.closed); return nil }

func mustEncode(t *testing.T, pdu ngapType.NGAPPDU) []byte {
	t.Helper()
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ranConfigOutcome(ack bool, cause *ngapType.Cause) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	if ack {
		pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
		pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
		pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeRANConfigurationUpdate
		pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentRANConfigurationUpdateAcknowledge
		pdu.SuccessfulOutcome.Value.RANConfigurationUpdateAcknowledge = new(ngapType.RANConfigurationUpdateAcknowledge)
		return pdu
	}
	pdu.Present = ngapType.NGAPPDUPresentUnsuccessfulOutcome
	pdu.UnsuccessfulOutcome = new(ngapType.UnsuccessfulOutcome)
	pdu.UnsuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeRANConfigurationUpdate
	pdu.UnsuccessfulOutcome.Value.Present = ngapType.UnsuccessfulOutcomePresentRANConfigurationUpdateFailure
	f := new(ngapType.RANConfigurationUpdateFailure)
	pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure = f
	if cause != nil {
		ie := ngapType.RANConfigurationUpdateFailureIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDCause
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.RANConfigurationUpdateFailureIEsPresentCause
		ie.Value.Cause = cause
		f.ProtocolIEs.List = append(f.ProtocolIEs.List, ie)
	}
	return pdu
}

// The exchange sends the update on stream 0 and returns the procedure's
// outcome: ACKNOWLEDGE, or FAILURE with its cause. A PDU of another
// procedure before it is dropped and counted. No outcome before the
// deadline, a read error and a write error are errors.
func TestRANConfigurationUpdateExchange(t *testing.T) {
	ack := ranConfigOutcome(true, nil)
	misc := &ngapType.Cause{Present: ngapType.CausePresentMisc, Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnspecified}}
	tests := []struct {
		name        string
		replies     []scriptReply
		writeErr    error
		wantAck     bool
		wantCause   string
		wantSkipped int
		wantErr     string
		wantTimeout bool
	}{
		{name: "acknowledge", replies: []scriptReply{{payload: mustEncode(t, ack)}}, wantAck: true},
		{name: "failure with a cause", replies: []scriptReply{{payload: mustEncode(t, ranConfigOutcome(false, misc))}}, wantCause: "misc(5)"},
		{name: "failure without a cause", replies: []scriptReply{{payload: mustEncode(t, ranConfigOutcome(false, nil))}}, wantCause: "absent"},
		{name: "another PDU first", replies: []scriptReply{{payload: mustEncode(t, BuildNGReset())}, {payload: mustEncode(t, ack)}}, wantAck: true, wantSkipped: 1},
		{name: "no outcome", wantErr: "RAN configuration update: waiting for NGAP PDU", wantTimeout: true},
		{name: "another PDU, then nothing", replies: []scriptReply{{payload: mustEncode(t, BuildNGReset())}}, wantErr: "waiting for NGAP PDU", wantTimeout: true},
		{name: "association closed", replies: []scriptReply{{err: io.EOF}}, wantErr: "EOF"},
		{name: "write fails", writeErr: errors.New("broken pipe"), wantErr: "broken pipe"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newScriptTransport(tc.replies...)
			tr.writeErr = tc.writeErr
			defer func() { _ = tr.Close() }()
			pdu, err := BuildRANConfigurationUpdate(WithRANNodeName("orbit-gnb-1"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			res, err := RANConfigurationUpdate(ctx, tr, pdu)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if errors.Is(err, context.DeadlineExceeded) != tc.wantTimeout {
					t.Errorf("errors.Is(err, DeadlineExceeded) = %v, want %v (%v)", !tc.wantTimeout, tc.wantTimeout, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Acknowledged != tc.wantAck || res.Cause != tc.wantCause || res.Skipped != tc.wantSkipped || res.PDU == nil {
				t.Errorf("result = %+v, want acknowledged %v cause %q skipped %d", res, tc.wantAck, tc.wantCause, tc.wantSkipped)
			}
			sent := <-tr.wrote
			if sent[0] != 0 || hex.EncodeToString(sent[1:]) != "002300140000010052400d05006f726269742d676e622d31" {
				t.Errorf("sent on stream %d: %x, want the update on stream 0", sent[0], sent[1:])
			}
		})
	}
}

// The outcome predicates tell the two outcomes from each other, from other
// PDUs and from nil.
func TestIsRANConfigurationUpdateOutcome(t *testing.T) {
	ack, fail, reset := ranConfigOutcome(true, nil), ranConfigOutcome(false, nil), BuildNGReset()
	tests := []struct {
		name              string
		pdu               *ngapType.NGAPPDU
		wantAck, wantFail bool
	}{
		{name: "acknowledge", pdu: &ack, wantAck: true},
		{name: "failure", pdu: &fail, wantFail: true},
		{name: "another PDU", pdu: &reset},
		{name: "nil"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRANConfigurationUpdateAcknowledge(tc.pdu); got != tc.wantAck {
				t.Errorf("IsRANConfigurationUpdateAcknowledge = %v, want %v", got, tc.wantAck)
			}
			if got := IsRANConfigurationUpdateFailure(tc.pdu); got != tc.wantFail {
				t.Errorf("IsRANConfigurationUpdateFailure = %v, want %v", got, tc.wantFail)
			}
		})
	}
}
