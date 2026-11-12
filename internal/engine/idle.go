package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

// CM-IDLE and back (TS 23.502 4.2.6 AN release, 4.2.3.2 UE triggered and
// 4.2.3.3 network triggered Service Request). Manager.Idle releases a
// registered UE's NAS signalling connection and N3 tunnels by an AN release
// and keeps its gNB's N2 association, on which the AMF pages the UE
// (Manager.ObservePaging). Manager.ServiceRequest brings the UE back and
// reactivates its PDU sessions' user plane; Manager.Deregister works from
// either state.

// IdleResult reports an AN release.
type IdleResult struct {
	// Requested is when the UE CONTEXT RELEASE REQUEST was sent and
	// Released when the core's UE CONTEXT RELEASE COMMAND was read.
	Requested, Released time.Time
	// Cause is the command's cause as "group/value" (gnb.ReleaseCommandCause).
	Cause string
	// STMSI is the identity the core pages the UE by.
	STMSI gnb.STMSI
}

// Idle reports whether the UE is CM-IDLE.
func (s *Session) Idle() bool {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.idle
}

// userPlaneDown reports whether the UE's N3 tunnel is released and not yet
// reactivated.
func (s *Session) userPlaneDown() bool {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.upDown
}

// setConnection records the UE's connection state: CM-IDLE or connected,
// and whether its user plane is down.
func (s *Session) setConnection(idle, upDown bool) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	s.idle, s.upDown = idle, upDown
}

// STMSI is the UE's 5G-S-TMSI, from the 5G-GUTI the Registration Accept
// assigned; false when the core assigned none.
func (s *Session) STMSI() (gnb.STMSI, bool) {
	t, err := gnb.STMSIFromGUTI(s.guti)
	return t, err == nil
}

// sendInitial sends nasPDU in an Initial UE Message that opens a new NAS
// signalling connection for a CM-IDLE UE: a fresh RAN-UE-NGAP-ID, and the
// 5G-S-TMSI by which the AMF finds the UE's context.
func (s *Session) sendInitial(nasPDU []byte, opts ...gnb.InitialUEOption) error {
	stmsi, ok := s.STMSI()
	if !ok {
		return fmt.Errorf("no 5G-GUTI (AMF assigned none in Registration Accept)")
	}
	s.ranID++
	pdu, err := gnb.BuildInitialUEMessage(s.ServingGNB(), s.ranID, nasPDU, append([]gnb.InitialUEOption{gnb.WithSTMSI(stmsi)}, opts...)...)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}

// completeRelease answers a UE CONTEXT RELEASE COMMAND with UE CONTEXT
// RELEASE COMPLETE. A command whose ID pair carries the UE's RAN-UE-NGAP-ID
// is answered with that pair (the AMF may have assigned a new
// AMF-UE-NGAP-ID); one that names no pair, or another RAN-UE-NGAP-ID, is
// answered with the session's own IDs. sessions are the PDU sessions the
// Complete lists as having had an active N3 user plane (an AN release).
func (s *Session) completeRelease(cmd *ngapType.NGAPPDU, sessions ...int64) error {
	amfID, ranID := s.amfID, s.ranID
	if a, r, ok := releaseCommandIDs(cmd); ok && r == s.ranID {
		amfID = a
	}
	return gnb.SendPDU(s.conn, ueStream, gnb.BuildUEContextReleaseComplete(amfID, ranID, sessions...))
}

// activeSessionIDs is the UE's PDU sessions whose N3 user plane is up: what
// an AN release names in its PDU Session Resource Lists, so that the AMF has
// the SMF deactivate them (TS 23.502 4.2.6 steps 1, 4 and 5). A core that
// follows those steps to the letter leaves an unlisted session's downlink
// pointed at the released gNB tunnel, and then never pages for it.
func (s *Session) activeSessionIDs() []int64 {
	if s.userPlaneDown() {
		return nil
	}
	held := s.heldSessions()
	ids := make([]int64, 0, len(held))
	for _, sr := range held {
		ids = append(ids, int64(sr.PDUSessionID))
	}
	return ids
}

// heldSessions is the UE's PDU sessions less the ones a UE-requested
// release ended: the sessions it still holds.
func (s *Session) heldSessions() []SessionResult {
	if s.Result == nil {
		return nil
	}
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	var out []SessionResult
	for _, sr := range s.Result.Sessions {
		if !s.pduReleased[sr.PDUSessionID] {
			out = append(out, sr)
		}
	}
	return out
}

// markPDUReleased records that a UE-requested release ended PDU session id.
func (s *Session) markPDUReleased(id uint8) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	if s.pduReleased == nil {
		s.pduReleased = map[uint8]bool{}
	}
	s.pduReleased[id] = true
}

// releaseCommandIDs is the NGAP ID pair of a UE CONTEXT RELEASE COMMAND.
func releaseCommandIDs(pdu *ngapType.NGAPPDU) (amfID, ranID int64, ok bool) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.UEContextReleaseCommand == nil {
		return 0, 0, false
	}
	for _, ie := range pdu.InitiatingMessage.Value.UEContextReleaseCommand.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDUENGAPIDs && ie.Value.UENGAPIDs != nil &&
			ie.Value.UENGAPIDs.Present == ngapType.UENGAPIDsPresentUENGAPIDPair && ie.Value.UENGAPIDs.UENGAPIDPair != nil {
			p := ie.Value.UENGAPIDs.UENGAPIDPair
			return p.AMFUENGAPID.Value, p.RANUENGAPID.Value, true
		}
	}
	return 0, 0, false
}

// isReleaseCommand reports whether pdu is a UE CONTEXT RELEASE COMMAND.
func isReleaseCommand(pdu *ngapType.NGAPPDU) bool {
	code, ok := procedureCode(pdu)
	return ok && code == ngapType.ProcedureCodeUEContextRelease && pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage
}

// session returns the registered UE's session.
func (m *Manager) session(supi string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[supi]
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	return sess, nil
}

// Idle moves a registered UE to CM-IDLE by an AN release (TS 23.502 4.2.6,
// TS 38.413 8.3.2 and 8.3.3): the gNB sends a UE CONTEXT RELEASE REQUEST
// (cause radio network, release due to NG-RAN generated reason), waits for
// the core's UE CONTEXT RELEASE COMMAND until ctx ends, and answers it with
// UE CONTEXT RELEASE COMPLETE. The UE's app sessions end and its N3 data
// path closes; the gNB's N2 association stays up, so the core can page the
// UE on it. The UE needs a 5G-GUTI, the identity it is paged by and comes
// back with.
func (m *Manager) Idle(ctx context.Context, supi string) (*IdleResult, error) {
	sess, err := m.session(supi)
	if err != nil {
		return nil, err
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if sess.Idle() {
		return nil, fmt.Errorf("UE %s is already CM-IDLE", supi)
	}
	stmsi, ok := sess.STMSI()
	if !ok {
		return nil, fmt.Errorf("UE %s has no 5G-GUTI: the core could not page it in CM-IDLE", supi)
	}
	if d, ok := sess.conn.(interface{ Drain() int }); ok {
		d.Drain()
	}
	sess.dropHeld()
	res := &IdleResult{Requested: time.Now(), STMSI: stmsi}
	active := sess.activeSessionIDs()
	if err := gnb.SendPDU(sess.conn, ueStream, gnb.BuildUEContextReleaseRequest(sess.amfID, sess.ranID, active...)); err != nil {
		return nil, fmt.Errorf("send UE Context Release Request: %w", err)
	}
	for {
		pdu, err := sess.readPDU(ctx)
		if err != nil {
			return nil, fmt.Errorf("no UE Context Release Command after the UE Context Release Request: %w", err)
		}
		if !isReleaseCommand(pdu) {
			continue
		}
		res.Released = time.Now()
		res.Cause, _ = gnb.ReleaseCommandCause(pdu)
		if err := sess.completeRelease(pdu, active...); err != nil {
			return nil, fmt.Errorf("send UE Context Release Complete: %w", err)
		}
		break
	}
	m.releaseAppSessions(supi)
	sess.closeDataPath()
	sess.setConnection(true, true)
	sess.setState(StateIdle)
	m.hub.publish(StateEvent{SUPI: supi, State: StateIdle, Detail: "AN release complete; UE is CM-IDLE"})
	return res, nil
}

// PagingEvent is one NGAP PAGING read on an idle UE's association.
type PagingEvent struct {
	At time.Time
	gnb.Paging
	// ForUE is true when the UE Paging Identity is the UE's 5G-S-TMSI.
	ForUE bool
}

// ObservePaging waits for the next NGAP PAGING on a CM-IDLE UE's N2
// association and returns it, whichever UE it names (PagingEvent.ForUE).
// Other downlink PDUs are skipped. It ends with ctx's error when none
// arrives; a wait that ends that way loses no PDU.
func (m *Manager) ObservePaging(ctx context.Context, supi string) (*PagingEvent, error) {
	sess, err := m.session(supi)
	if err != nil {
		return nil, err
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if !sess.Idle() {
		return nil, fmt.Errorf("UE %s is not CM-IDLE", supi)
	}
	own, _ := sess.STMSI()
	for {
		pdu, err := sess.readPDU(ctx)
		if err != nil {
			return nil, fmt.Errorf("waiting for PAGING: %w", err)
		}
		p, ok := gnb.ParsePaging(pdu)
		if !ok {
			continue
		}
		return &PagingEvent{At: time.Now(), Paging: p, ForUE: p.HasSTMSI && p.STMSI == own}, nil
	}
}

// ServiceOption adjusts Manager.ServiceRequest.
type ServiceOption func(*serviceConfig)

type serviceConfig struct {
	mobileTerminated bool
	uplink           *uint16
	settle           time.Duration
}

// WithMobileTerminated sends the Service Request as the answer to paging:
// service type "mobile terminated services" and RRC establishment cause
// mt-Access. The default is service type "data" with cause mo-Data.
func WithMobileTerminated() ServiceOption {
	return func(c *serviceConfig) { c.mobileTerminated = true }
}

// WithUplinkDataStatus names the PDU sessions the request asks the core to
// reactivate (its Uplink data status). The default is every session the UE
// holds; none asks for the signalling connection only.
func WithUplinkDataStatus(pduSessionIDs ...uint8) ServiceOption {
	return func(c *serviceConfig) {
		var v uint16
		for _, id := range pduSessionIDs {
			v |= 1 << (id & 0x0f)
		}
		c.uplink = &v
	}
}

// WithUserPlaneWait bounds how long ServiceRequest keeps reading, after the
// Service Accept (or Reject), for the user-plane setup of the sessions it
// asked for (or the release). The default is 2s.
func WithUserPlaneWait(d time.Duration) ServiceOption {
	return func(c *serviceConfig) { c.settle = d }
}

// defaultUserPlaneWait is WithUserPlaneWait's default.
const defaultUserPlaneWait = 2 * time.Second

// ServiceResult reports a Service Request procedure as recorded data: what
// the core answered and which sessions' user plane it set up again. It
// judges nothing.
type ServiceResult struct {
	// Sent is when the Service Request left; Answered when the Service
	// Accept or Reject was read (zero when neither came).
	Sent, Answered time.Time
	// Accepted and Rejected say which answer came; RejectCause is the
	// Reject's 5GMM cause.
	Accepted, Rejected bool
	RejectCause        uint8
	// Requested is the Uplink data status the request carried: bit n set
	// for PDU session ID n.
	Requested uint16
	// Accept is the Service Accept's content, nil without one.
	Accept *ue.ServiceAccept
	// ContextSetup is true when the core ran Initial Context Setup.
	ContextSetup bool
	// Reactivated are the PDU sessions whose N3 tunnel the core set up
	// again, with their new endpoints.
	Reactivated []SessionResult
	// Released is true when the core released the UE context during the
	// procedure (as after a Reject); the UE is then still CM-IDLE.
	Released bool
}

// UserPlane reports whether the core set up the N3 tunnel of PDU session id
// again.
func (r *ServiceResult) UserPlane(id uint8) bool {
	for _, s := range r.Reactivated {
		if s.PDUSessionID == id {
			return true
		}
	}
	return false
}

// ErrServiceUnanswered is the error ServiceRequest returns when neither a
// Service Accept nor a Service Reject came before ctx ended.
var ErrServiceUnanswered = errors.New("the core answered the Service Request with neither Service Accept nor Service Reject")

// ServiceRequest brings a CM-IDLE UE back to CM-CONNECTED (TS 24.501 5.6.1,
// TS 23.502 4.2.3.2): it sends a Service Request in an Initial UE Message
// naming the UE by its 5G-S-TMSI, integrity-protected, with the complete
// request ciphered in its NAS message container when it carries an Uplink
// data status or PDU session status (TS 24.501 4.4.6). It answers the
// core's Initial Context Setup and PDU Session Resource Setup with the
// gNB's new downlink tunnels, and returns what the core did once the
// request is answered and the asked-for sessions are set up, or the
// user-plane wait has passed. An Accept makes the UE connected again; its
// data path reopens on the reactivated tunnels at the next use. The error
// is ErrServiceUnanswered when no answer came; a Reject is a result, not
// an error.
func (m *Manager) ServiceRequest(ctx context.Context, supi string, opts ...ServiceOption) (*ServiceResult, error) {
	cfg := serviceConfig{settle: defaultUserPlaneWait}
	for _, o := range opts {
		o(&cfg)
	}
	sess, err := m.session(supi)
	if err != nil {
		return nil, err
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if !sess.Idle() {
		return nil, fmt.Errorf("UE %s is not CM-IDLE", supi)
	}
	if sess.sec == nil {
		return nil, fmt.Errorf("no security context")
	}

	var held uint16
	for _, s := range sess.heldSessions() {
		held |= 1 << (s.PDUSessionID & 0x0f)
	}
	p := ue.ServiceRequestParams{GUTI: sess.guti, NgKSI: sess.ngksi, ServiceType: ue.ServiceTypeData,
		UplinkDataStatus: held, PDUSessionStatus: held}
	if cfg.uplink != nil {
		p.UplinkDataStatus = *cfg.uplink
	}
	initialOpt := gnb.WithMOData()
	switch {
	case cfg.mobileTerminated:
		p.ServiceType, initialOpt = ue.ServiceTypeMobileTerminated, gnb.WithMTAccess()
	case p.UplinkDataStatus == 0:
		p.ServiceType = ue.ServiceTypeSignalling
	}
	plain, err := ue.BuildServiceRequest(p)
	if err != nil {
		return nil, err
	}
	if p.HasNonCleartext() {
		container := append([]byte(nil), plain...)
		if err := sess.sec.CipherContainer(container); err != nil {
			return nil, err
		}
		if plain, err = ue.BuildServiceRequestWithContainer(p, container); err != nil {
			return nil, err
		}
	}
	wrapped, err := sess.sec.EncodeSecureRaw(plain, nas.SecHdrIntegrity)
	if err != nil {
		return nil, fmt.Errorf("wrap Service Request: %w", err)
	}
	if d, ok := sess.conn.(interface{ Drain() int }); ok {
		d.Drain()
	}
	sess.dropHeld()
	res := &ServiceResult{Sent: time.Now(), Requested: p.UplinkDataStatus}
	if err := sess.sendInitial(wrapped, initialOpt); err != nil {
		return nil, fmt.Errorf("send Service Request: %w", err)
	}

	sr := &serviceRun{m: m, sess: sess, res: res}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var settle *time.Timer
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()
	for !sr.complete() {
		pdu, err := sess.readPDU(rctx)
		if err != nil {
			if res.Accepted || res.Rejected {
				break // the user-plane wait is over, or ctx ended after the answer
			}
			return nil, fmt.Errorf("%w: %w", ErrServiceUnanswered, err)
		}
		if err := sr.handle(pdu); err != nil {
			return nil, err
		}
		if (res.Accepted || res.Rejected) && settle == nil {
			settle = time.AfterFunc(cfg.settle, cancel)
		}
	}
	sr.apply()
	return res, nil
}

// serviceRun is one Service Request procedure's reading state.
type serviceRun struct {
	m    *Manager
	sess *Session
	res  *ServiceResult
}

// complete reports whether nothing more is expected: the UE was released,
// or the request was accepted and every asked-for session is set up.
func (r *serviceRun) complete() bool {
	if r.res.Released {
		return true
	}
	if !r.res.Accepted {
		return false
	}
	for id := uint8(0); id < 16; id++ {
		if r.res.Requested&(1<<id) != 0 && !r.res.UserPlane(id) {
			return false
		}
	}
	return true
}

// handle processes one downlink PDU of the procedure.
func (r *serviceRun) handle(pdu *ngapType.NGAPPDU) error {
	s := r.sess
	if isReleaseCommand(pdu) {
		r.res.Released = true
		return s.completeRelease(pdu)
	}
	if amfID, ranID, sessions, err := gnb.ParsePDUSessionResourceSetupRequest(pdu); err == nil {
		s.amfID = amfID
		teid := r.teids(sessions)
		resp, err := gnb.BuildPDUSessionResourceSetupResponse(amfID, ranID, sessions, s.gnbN3OrLoopback(), teid)
		if err != nil {
			return err
		}
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return fmt.Errorf("send PDU Session Resource Setup Response: %w", err)
		}
		r.record(sessions, teid)
		return nil
	}
	dl, err := gnb.ParseDownlink(pdu)
	if err != nil {
		return nil // not a PDU this procedure reads
	}
	if dl.HasAMFID {
		s.amfID = dl.AMFUENGAPID
	}
	switch dl.Procedure {
	case ngapType.ProcedureCodeInitialContextSetup:
		if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
			return nil
		}
		if dl.UESecurityCapabilities != nil {
			s.setUESecCaps(dl.UESecurityCapabilities)
		}
		sessions, err := gnb.ParseInitialContextSetupSessions(pdu)
		if err != nil {
			return err
		}
		teid := r.teids(sessions)
		resp, err := gnb.BuildInitialContextSetupResponseSessions(s.amfID, s.ranID, sessions, s.gnbN3OrLoopback(), teid)
		if err != nil {
			return err
		}
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return fmt.Errorf("send Initial Context Setup Response: %w", err)
		}
		r.res.ContextSetup = true
		r.record(sessions, teid)
	case ngapType.ProcedureCodeDownlinkNASTransport:
	default:
		return nil
	}
	if len(dl.NASPDU) == 0 {
		return nil
	}
	return r.handleNAS(dl.NASPDU)
}

// teids allocates a gNB downlink TEID for each session of one setup.
func (r *serviceRun) teids(sessions []gnb.PDUSessionResource) func(int64) uint32 {
	t := make(map[int64]uint32, len(sessions))
	for _, s := range sessions {
		t[s.PDUSessionID] = allocDLTEID()
	}
	return func(id int64) uint32 { return t[id] }
}

// record notes the sessions one setup reactivated.
func (r *serviceRun) record(sessions []gnb.PDUSessionResource, teid func(int64) uint32) {
	for _, n := range sessions {
		sr := SessionResult{PDUSessionID: uint8(n.PDUSessionID), UPFAddress: n.UPFAddress, UPFTEID: n.UPFTEID, DLTEID: teid(n.PDUSessionID)}
		if len(n.QFIs) > 0 {
			sr.QFI = uint8(n.QFIs[0])
		}
		for _, old := range r.sess.Result.Sessions {
			if old.PDUSessionID == sr.PDUSessionID {
				sr.DNN, sr.Slice, sr.IPv4 = old.DNN, old.Slice, old.IPv4
				if sr.QFI == 0 {
					sr.QFI = old.QFI
				}
			}
		}
		r.res.Reactivated = append(r.res.Reactivated, sr)
	}
}

// handleNAS reads the procedure's downlink NAS: the Service Accept or
// Reject. Anything else, and a PDU that does not verify or decode, is
// skipped; an Authentication Request is an error, since the procedure does
// not re-authenticate.
func (r *serviceRun) handleNAS(raw []byte) error {
	var msg *f5nas.Message
	var err error
	if len(raw) >= 2 && f5nas.GetSecurityHeaderType(raw) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(raw)
	} else {
		msg, err = r.sess.sec.DecodeSecure(raw)
	}
	if err != nil || msg.GmmMessage == nil {
		r.m.log.Debug("service request: skipping a downlink NAS PDU that did not decode", "supi", r.sess.SUPI, "err", err)
		return nil
	}
	switch msg.GmmHeader.GetMessageType() {
	case f5nas.MsgTypeServiceAccept:
		acc, err := ue.ParseServiceAccept(msg)
		if err != nil {
			return err
		}
		r.res.Accepted, r.res.Accept, r.res.Answered = true, acc, time.Now()
	case f5nas.MsgTypeServiceReject:
		cause, err := ue.ParseServiceReject(msg)
		if err != nil {
			return err
		}
		r.res.Rejected, r.res.RejectCause, r.res.Answered = true, cause, time.Now()
	case f5nas.MsgTypeAuthenticationRequest:
		return errors.New("the core re-authenticated the UE during the Service Request, which is not supported")
	}
	return nil
}

// apply moves the session to what the procedure reached: connected after
// an Accept that was not followed by a release, with the reactivated
// sessions' tunnels as its data-path endpoints.
func (r *serviceRun) apply() {
	s, res := r.sess, r.res
	if !res.Accepted || res.Released {
		return
	}
	s.dpMu.Lock()
	for _, n := range res.Reactivated {
		for i := range s.Result.Sessions {
			if s.Result.Sessions[i].PDUSessionID == n.PDUSessionID {
				s.Result.Sessions[i] = n
			}
		}
	}
	s.dpMu.Unlock()
	// The user plane is up when any session the UE holds was reactivated;
	// the first of them gives the result's data-path fields.
	up := false
	for _, f := range s.heldSessions() {
		if res.UserPlane(f.PDUSessionID) {
			up = true
			s.dpMu.Lock()
			s.Result.UPFAddress, s.Result.UPFTEID, s.Result.DLTEID, s.Result.QFI = f.UPFAddress, f.UPFTEID, f.DLTEID, f.QFI
			s.dpMu.Unlock()
			break
		}
	}
	s.setConnection(false, !up)
	state, detail := StateRegistered, "Service Accept; UE is CM-CONNECTED, user plane not reactivated"
	if up {
		state, detail = StateSessionActive, fmt.Sprintf("Service Accept; %d PDU session(s) reactivated", len(res.Reactivated))
	}
	s.setState(state)
	r.m.hub.publish(StateEvent{SUPI: s.SUPI, State: state, Detail: detail})
}

// gnbN3OrLoopback is the gNB N3 address reported in a setup response; a UE
// registered without one (control plane only) reports loopback, as its
// attach did.
func (s *Session) gnbN3OrLoopback() string {
	if s.gnbN3 != "" {
		return s.gnbN3
	}
	return "127.0.0.1"
}
