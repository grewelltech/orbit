package gnb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// scriptTr is an association the test feeds: ReadMsg returns what push
// queued, and an error once closed.
type scriptTr struct {
	in     chan scripted
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	writes []uint16 // the stream of each write
}

type scripted struct {
	b    []byte
	ppid uint32
}

func newScriptTr() *scriptTr {
	return &scriptTr{in: make(chan scripted, 1024), closed: make(chan struct{})}
}

func (s *scriptTr) push(b []byte)               { s.in <- scripted{b, sctp.PPIDNGAP} }
func (s *scriptTr) pushPPID(b []byte, p uint32) { s.in <- scripted{b, p} }

func (s *scriptTr) ReadMsg([]byte) ([]byte, uint16, uint32, error) {
	select {
	case m := <-s.in:
		return m.b, 1, m.ppid, nil
	case <-s.closed:
		return nil, 0, 0, io.EOF
	}
}

func (s *scriptTr) WriteNGAP(stream uint16, _ []byte) error {
	s.mu.Lock()
	s.writes = append(s.writes, stream)
	s.mu.Unlock()
	return nil
}

func (s *scriptTr) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func encodePDU(t *testing.T, pdu ngapType.NGAPPDU) []byte {
	t.Helper()
	b, err := ngap.Encode(pdu)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dlNAS is a DOWNLINK NAS TRANSPORT for the ID pair, its NAS PDU a marker
// byte so a reader can tell messages apart.
func dlNAS(t *testing.T, amfID, ranID int64, marker byte) []byte {
	t.Helper()
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = &ngapType.InitiatingMessage{}
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeDownlinkNASTransport
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentDownlinkNASTransport
	m := &ngapType.DownlinkNASTransport{}
	pdu.InitiatingMessage.Value.DownlinkNASTransport = m
	ies := []ngapType.DownlinkNASTransportIEs{{}, {}, {}}
	ies[0].Id.Value, ies[0].Value.Present = ngapType.ProtocolIEIDAMFUENGAPID, ngapType.DownlinkNASTransportIEsPresentAMFUENGAPID
	ies[0].Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	ies[1].Id.Value, ies[1].Value.Present = ngapType.ProtocolIEIDRANUENGAPID, ngapType.DownlinkNASTransportIEsPresentRANUENGAPID
	ies[1].Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	ies[2].Id.Value, ies[2].Value.Present = ngapType.ProtocolIEIDNASPDU, ngapType.DownlinkNASTransportIEsPresentNASPDU
	ies[2].Value.NASPDU = &ngapType.NASPDU{Value: aper.OctetString{0x7e, 0x00, marker}}
	m.ProtocolIEs.List = ies
	return encodePDU(t, pdu)
}

// hoRequest is a HANDOVER REQUEST, which names its UE by AMF-UE-NGAP-ID only.
func hoRequest(t *testing.T, amfID int64) []byte {
	t.Helper()
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = &ngapType.InitiatingMessage{}
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeHandoverResourceAllocation
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverRequest
	m := &ngapType.HandoverRequest{}
	pdu.InitiatingMessage.Value.HandoverRequest = m
	ie := ngapType.HandoverRequestIEs{}
	ie.Id.Value, ie.Value.Present = ngapType.ProtocolIEIDAMFUENGAPID, ngapType.HandoverRequestIEsPresentAMFUENGAPID
	ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	m.ProtocolIEs.List = []ngapType.HandoverRequestIEs{ie}
	return encodePDU(t, pdu)
}

// paging is a non-UE-associated PAGING.
func paging(t *testing.T) []byte {
	t.Helper()
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = &ngapType.InitiatingMessage{}
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePaging
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPaging
	m := &ngapType.Paging{}
	pdu.InitiatingMessage.Value.Paging = m
	ie := ngapType.PagingIEs{}
	ie.Id.Value, ie.Value.Present = ngapType.ProtocolIEIDUEPagingIdentity, ngapType.PagingIEsPresentUEPagingIdentity
	ie.Value.UEPagingIdentity = &ngapType.UEPagingIdentity{Present: ngapType.UEPagingIdentityPresentFiveGSTMSI,
		FiveGSTMSI: &ngapType.FiveGSTMSI{
			AMFSetID:   ngapType.AMFSetID{Value: aper.BitString{Bytes: []byte{0x01, 0x00}, BitLength: 10}},
			AMFPointer: ngapType.AMFPointer{Value: aper.BitString{Bytes: []byte{0x04}, BitLength: 6}},
			FiveGTMSI:  ngapType.FiveGTMSI{Value: aper.OctetString{1, 2, 3, 4}}}}
	m.ProtocolIEs.List = []ngapType.PagingIEs{ie}
	return encodePDU(t, pdu)
}

// marker reads the marker byte back from a DOWNLINK NAS TRANSPORT.
func marker(t *testing.T, pdu *ngapType.NGAPPDU) byte {
	t.Helper()
	dl, err := ParseDownlink(pdu)
	if err != nil || len(dl.NASPDU) < 3 {
		t.Fatalf("not a marked DOWNLINK NAS TRANSPORT: %+v %v", dl, err)
	}
	return dl.NASPDU[2]
}

// startedSession is a shared session whose read loop runs, as Dial leaves it.
func startedSession(tr Transport, cfg Config) *Session {
	s := newSession(tr, cfg)
	go s.readLoop()
	return s
}

func readWithin(t *testing.T, l *UETransport, d time.Duration) (*ngapType.NGAPPDU, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return ReadPDU(ctx, l)
}

// Routing on a shared association: by RAN-UE-NGAP-ID, then by an
// AMF-UE-NGAP-ID learned from routed PDUs; a non-UE-associated PDU to the
// handler; anything else (an unknown UE, an undecodable PDU, a non-NGAP
// PPID) is dropped and counted rather than misdelivered.
func TestSharedSessionRouting(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{Name: "g"})
	defer func() { _ = s.Close() }()
	a, ranA := s.NewUE()
	b, ranB := s.NewUE()
	var nonUE []*ngapType.NGAPPDU
	var mu sync.Mutex
	s.SetNonUEHandler(func(p *ngapType.NGAPPDU) {
		mu.Lock()
		nonUE = append(nonUE, p)
		mu.Unlock()
	})

	tr.push(dlNAS(t, 10, ranA, 'a'))
	tr.push(dlNAS(t, 20, ranB, 'b'))
	tr.push(dlNAS(t, 99, 999, 'x')) // no such RAN ID
	tr.push(hoRequest(t, 20))       // learned: AMF 20 is lane B
	tr.push(hoRequest(t, 77))       // never learned
	tr.push(paging(t))
	tr.push([]byte{0xff, 0xff})              // undecodable
	tr.pushPPID(dlNAS(t, 10, ranA, 'p'), 46) // a non-NGAP PPID

	if p, err := readWithin(t, a, time.Second); err != nil || marker(t, p) != 'a' {
		t.Fatalf("lane A: %v", err)
	}
	if p, err := readWithin(t, b, time.Second); err != nil || marker(t, p) != 'b' {
		t.Fatalf("lane B: %v", err)
	}
	p, err := readWithin(t, b, time.Second)
	if err != nil || p.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentHandoverRequest {
		t.Fatalf("lane B second PDU = %v, %v; want the HANDOVER REQUEST routed by learned AMF ID", p, err)
	}
	deadline := time.Now().Add(time.Second)
	for s.Dropped() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Dropped(); got != 4 {
		t.Errorf("Dropped = %d, want 4 (unknown RAN ID, unlearned AMF ID, undecodable, non-NGAP PPID)", got)
	}
	mu.Lock()
	if len(nonUE) != 1 || nonUE[0].InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentPaging {
		t.Errorf("non-UE handler got %d PDUs, want the PAGING", len(nonUE))
	}
	mu.Unlock()
	for _, l := range []*UETransport{a, b} {
		if _, err := readWithin(t, l, 30*time.Millisecond); err == nil {
			t.Error("a lane got a PDU meant for no one")
		}
	}
}

// A dedicated association delivers everything no lane claims to its one
// lane, as a direct read would: unknown IDs, non-UE PDUs, an undecodable
// PDU (a decode error at read) and a non-NGAP PPID (an error at read).
// Writes keep the caller's stream, and closing the lane closes the
// association.
func TestDedicatedSession(t *testing.T) {
	tr := newScriptTr()
	l := Dedicated(tr, Config{Name: "g"}, 1)
	tr.push(dlNAS(t, 5, 1, 'a'))
	tr.push(dlNAS(t, 5, 42, 'b')) // another RAN ID: still this UE's association
	tr.push(hoRequest(t, 9))
	tr.push(paging(t))
	tr.push([]byte{0xff, 0xff})
	tr.pushPPID(dlNAS(t, 5, 1, 'c'), 46)

	for _, want := range []byte{'a', 'b'} {
		p, err := readWithin(t, l, time.Second)
		if err != nil || marker(t, p) != want {
			t.Fatalf("want marker %c: %v", want, err)
		}
	}
	for _, want := range []int{ngapType.InitiatingMessagePresentHandoverRequest, ngapType.InitiatingMessagePresentPaging} {
		p, err := readWithin(t, l, time.Second)
		if err != nil || p.InitiatingMessage.Value.Present != want {
			t.Fatalf("want message %d: %v, %v", want, p, err)
		}
	}
	if _, err := readWithin(t, l, time.Second); err == nil {
		t.Error("an undecodable PDU read without error")
	}
	if _, err := readWithin(t, l, time.Second); err == nil || err.Error() != "non-NGAP PPID" {
		t.Errorf("non-NGAP PPID read = %v", err)
	}
	if err := SendPDU(l, 7, BuildInitialContextSetupResponse(1, 1)); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	if len(tr.writes) != 1 || tr.writes[0] != 7 {
		t.Errorf("writes on streams %v, want [7]", tr.writes)
	}
	tr.mu.Unlock()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.closed:
	case <-time.After(time.Second):
		t.Fatal("closing the dedicated lane left the association open")
	}
}

// A read that ends on its context leaves no read behind: the PDU that
// arrives next goes to the next read, not to a goroutine nobody waits on
// (the abandoned-read loss of a direct gnb.ReadPDU).
func TestLaneReadContextLosesNothing(t *testing.T) {
	tr := newScriptTr()
	l := Dedicated(tr, Config{}, 1)
	defer func() { _ = l.Close() }()
	for i := 0; i < 5; i++ {
		if _, err := readWithin(t, l, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("empty read %d = %v, want the deadline", i, err)
		}
	}
	tr.push(dlNAS(t, 1, 1, 'z'))
	p, err := readWithin(t, l, time.Second)
	if err != nil || marker(t, p) != 'z' {
		t.Fatalf("the PDU after timed-out reads: %v", err)
	}
}

// One UE's PDUs arrive in the order the AMF sent them.
func TestLaneOrder(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	defer func() { _ = s.Close() }()
	l, ran := s.NewUE()
	for i := 0; i < laneCapacity; i++ {
		tr.push(dlNAS(t, 1, ran, byte(i)))
	}
	for i := 0; i < laneCapacity; i++ {
		p, err := readWithin(t, l, time.Second)
		if err != nil || marker(t, p) != byte(i) {
			t.Fatalf("PDU %d: %v", i, err)
		}
	}
}

// A lane nobody reads fills, and the loop drops (and counts) what does not
// fit rather than stalling: another UE's PDUs behind them still arrive.
func TestFullLaneDoesNotStallTheAssociation(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	defer func() { _ = s.Close() }()
	_, ranSlow := s.NewUE()
	fast, ranFast := s.NewUE()
	const extra = 10
	for i := 0; i < laneCapacity+extra; i++ {
		tr.push(dlNAS(t, 1, ranSlow, 's'))
	}
	tr.push(dlNAS(t, 2, ranFast, 'f'))
	p, err := readWithin(t, fast, time.Second)
	if err != nil || marker(t, p) != 'f' {
		t.Fatalf("the other UE's PDU: %v", err)
	}
	if got := s.Dropped(); got != extra {
		t.Errorf("Dropped = %d, want %d", got, extra)
	}
}

// When the association closes, a waiting reader gets ErrAssociationClosed
// (wrapping the cause) instead of hanging, after what was already queued;
// ReadMsg reports the same.
func TestAssociationCloseWakesReaders(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	waiting, _ := s.NewUE()
	queued, ranQ := s.NewUE()
	tr.push(dlNAS(t, 1, ranQ, 'q'))
	deadline := time.Now().Add(time.Second)
	for len(queued.inbox) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := readWithin(t, waiting, 5*time.Second)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = tr.Close() // the peer goes away
	select {
	case err := <-errc:
		if !errors.Is(err, ErrAssociationClosed) || !errors.Is(err, io.EOF) {
			t.Errorf("waiting reader got %v, want ErrAssociationClosed wrapping EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reader hung after the association closed")
	}
	if p, err := readWithin(t, queued, time.Second); err != nil || marker(t, p) != 'q' {
		t.Errorf("queued PDU after close: %v", err)
	}
	if _, _, _, err := queued.ReadMsg(nil); !errors.Is(err, ErrAssociationClosed) {
		t.Errorf("ReadMsg after the drain = %v, want ErrAssociationClosed", err)
	}
}

// Many UEs run procedures on one association at once: each reads its own
// PDUs and writes while others do, and a Close mid-flight wakes them all.
// Run with -race.
func TestConcurrentProceduresOnOneAssociation(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	const ues, perUE = 8, 20
	lanes := make([]*UETransport, ues)
	rans := make([]int64, ues)
	for i := range lanes {
		lanes[i], rans[i] = s.NewUE()
	}
	var wg sync.WaitGroup
	errs := make(chan error, ues)
	for i := range lanes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < perUE; n++ {
				if err := SendPDU(lanes[i], 1, BuildInitialContextSetupResponse(1, 1)); err != nil {
					errs <- err
					return
				}
				p, err := readWithin(t, lanes[i], 2*time.Second)
				if err != nil {
					errs <- fmt.Errorf("UE %d read %d: %w", i, n, err)
					return
				}
				if got := marker(t, p); got != byte(n) {
					errs <- fmt.Errorf("UE %d read %d: marker %d", i, n, got)
					return
				}
			}
		}(i)
	}
	for n := 0; n < perUE; n++ {
		for i := range lanes {
			tr.push(dlNAS(t, int64(100+i), rans[i], byte(n)))
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// A second round blocked in reads, then the association closes.
	for i := range lanes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := readWithin(t, lanes[i], 5*time.Second); !errors.Is(err, ErrAssociationClosed) {
				t.Errorf("UE %d after close: %v", i, err)
			}
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	_ = s.Close()
	wg.Wait()
}

// A closed lane's learned AMF-UE-NGAP-ID pairing is forgotten, so a later
// PDU naming only that AMF ID is dropped rather than routed to a lane that
// is gone, and the index does not grow with UE churn.
func TestClosedLaneForgetsLearnedAMFID(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	defer func() { _ = s.Close() }()
	l, ran := s.NewUE()
	tr.push(dlNAS(t, 30, ran, 'a'))
	if p, err := readWithin(t, l, time.Second); err != nil || marker(t, p) != 'a' {
		t.Fatalf("lane read: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	idx, rev := len(s.amfIdx), len(s.amfOf)
	s.mu.Unlock()
	if idx != 0 || rev != 0 {
		t.Errorf("after Close the index holds %d and %d entries, want none", idx, rev)
	}
	tr.push(hoRequest(t, 30))
	deadline := time.Now().Add(time.Second)
	for s.Dropped() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Dropped(); got != 1 {
		t.Errorf("Dropped = %d, want 1 (the PDU for the forgotten AMF ID)", got)
	}
}

// A dedicated association reads every PDU that was already waiting on it
// when it was wrapped: the lane is the default route before anything reads.
func TestDedicatedReadsWhatWasAlreadyWaiting(t *testing.T) {
	tr := newScriptTr()
	const n = 32
	for i := 0; i < n; i++ {
		tr.push(dlNAS(t, 5, 1, byte(i)))
	}
	l := Dedicated(tr, Config{Name: "g"}, 1)
	defer func() { _ = l.Close() }()
	for i := 0; i < n; i++ {
		p, err := readWithin(t, l, time.Second)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := marker(t, p); got != byte(i) {
			t.Fatalf("read %d: marker %d", i, got)
		}
	}
	if d := l.s.Dropped(); d != 0 {
		t.Errorf("Dropped = %d, want 0", d)
	}
}

// When the AMF moves its ID from one UE to another, closing the first UE's
// lane leaves the second UE's routing in place.
func TestReassignedAMFIDSurvivesTheOldLaneClosing(t *testing.T) {
	tr := newScriptTr()
	s := startedSession(tr, Config{})
	defer func() { _ = s.Close() }()
	l1, ran1 := s.NewUE()
	l2, ran2 := s.NewUE()
	tr.push(dlNAS(t, 30, ran1, 'a'))
	if p, err := readWithin(t, l1, time.Second); err != nil || marker(t, p) != 'a' {
		t.Fatalf("first lane: %v", err)
	}
	tr.push(dlNAS(t, 30, ran2, 'b'))
	if p, err := readWithin(t, l2, time.Second); err != nil || marker(t, p) != 'b' {
		t.Fatalf("second lane: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	idx, rev := s.amfIdx[30], len(s.amfOf)
	s.mu.Unlock()
	if idx != ran2 || rev != 1 {
		t.Fatalf("after the first lane closed: amfIdx[30] = %d, %d reverse entries; want %d and 1", idx, rev, ran2)
	}
	tr.push(hoRequest(t, 30))
	p, err := readWithin(t, l2, time.Second)
	if err != nil || p.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentHandoverRequest {
		t.Fatalf("AMF-ID-only PDU for the second UE: %v, %v", p, err)
	}
}
