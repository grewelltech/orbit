package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// nasEvents collects the downlink NAS an attach's probe decoded.
type nasEvents struct {
	mu  sync.Mutex
	evs []NASEvent
}

func (n *nasEvents) add(ev NASEvent) {
	n.mu.Lock()
	n.evs = append(n.evs, ev)
	n.mu.Unlock()
}

func (n *nasEvents) list() []NASEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]NASEvent(nil), n.evs...)
}

// Every pair of 128-NIA1/2/3 and NEA0/1/2/3, over the real-SCTP mock front:
// the UE advertises just that pair, the mock selects it, and the attach
// registers, establishes a PDU session (the mock reads the ciphered uplink),
// decodes every downlink through OnNAS, and deregisters. The installed
// context is the selected pair, and every downlink after the Security Mode
// Command is protected with it.
func TestAttachEveryNASAlgorithmPair(t *testing.T) {
	const base = "127.0.61."
	upf, err := mockupf.Listen(base+"10:2152", mockupf.Options{})
	if err != nil {
		t.Skipf("cannot bind the stub UPF on %s10:2152: %v", base, err)
	}
	t.Cleanup(func() { _ = upf.Close() })
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	n := 0
	for _, ia := range []uint8{nas.NIA1, nas.NIA2, nas.NIA3} {
		for _, ea := range []uint8{nas.NEA0, nas.NEA1, nas.NEA2, nas.NEA3} {
			n++
			supi := fmt.Sprintf("2089301000075%02d", n)
			t.Run(fmt.Sprintf("NIA%d NEA%d", ia, ea), func(t *testing.T) {
				amf, err := mockamf.New(mockamf.Config{
					Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
					MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: upf, UPFAddress: base + "10",
				}, mockamf.WithGUTI(), mockamf.WithIntegrityOrder(ia), mockamf.WithCipheringOrder(ea))
				if err != nil {
					t.Fatal(err)
				}
				addr := sctpFront(t, amf)
				caps, err := ue.NewSecurityCapability([]uint8{ea}, []uint8{ia})
				if err != nil {
					t.Fatal(err)
				}
				id, err := ue.ParseIdentity(supi, "208", "93", "0")
				if err != nil {
					t.Fatal(err)
				}
				rec := &nasEvents{}
				m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: addr}, UEConfig{
					Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
					PDUSession:           &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
					GNBN3Addr:            base + "11",
					SecurityCapabilities: &caps,
					Probe:                &AttachProbe{OnNAS: rec.add},
				})
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				if !res.SessionActive {
					t.Fatalf("no PDU session: %+v", res)
				}
				sess, err := m.Status(supi)
				if err != nil {
					t.Fatal(err)
				}
				if sess.sec.IntegrityAlg != ia || sess.sec.CipheringAlg != ea {
					t.Errorf("installed context 128-NIA%d NEA%d, want 128-NIA%d NEA%d", sess.sec.IntegrityAlg, sess.sec.CipheringAlg, ia, ea)
				}
				var sawSMC, sawAccept bool
				for _, ev := range rec.list() {
					if ev.Err != nil {
						t.Errorf("downlink %x did not decode: %v", ev.Raw, ev.Err)
						continue
					}
					switch ev.Msg.GmmHeader.GetMessageType() {
					case f5nas.MsgTypeSecurityModeCommand:
						sel, err := ue.ParseSecurityModeCommand(ev.Msg)
						if err != nil || sel.Integrity != ia || sel.Ciphering != ea {
							t.Errorf("Security Mode Command selected %+v (%v), want 128-NIA%d NEA%d", sel, err, ia, ea)
						}
						sawSMC = true
					case f5nas.MsgTypeRegistrationAccept:
						if ev.Raw[1] != nas.SecHdrIntegrityCiphered {
							t.Errorf("Registration Accept security header %d, want integrity protected and ciphered", ev.Raw[1])
						}
						sawAccept = true
					}
				}
				if !sawSMC || !sawAccept {
					t.Errorf("OnNAS saw Security Mode Command %v, Registration Accept %v; want both", sawSMC, sawAccept)
				}
				if err := m.Deregister(ctx, supi); err != nil {
					t.Errorf("Deregister: %v", err)
				}
			})
		}
	}
}

// The Security Mode Command's algorithms are refused when the UE did not
// advertise them, or when NIA0 is selected even though it was advertised; an
// SMC whose MAC is wrong under the selected algorithm fails too. The default
// pair still attaches.
func TestAttachSecurityModeCommandAlgorithms(t *testing.T) {
	tests := []struct {
		name    string
		opts    []mockamf.Option
		ea, ia  []uint8 // advertised; nil is the default (NEA0, 128-NIA2)
		wantErr error
	}{
		{name: "default pair"},
		{name: "integrity not advertised", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA1)}, wantErr: ErrUnsupportedAlgorithms},
		{name: "ciphering not advertised", opts: []mockamf.Option{mockamf.WithSMCCiphering(nas.NEA2)}, wantErr: ErrUnsupportedAlgorithms},
		{name: "NIA0 although advertised", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA0)},
			ea: []uint8{nas.NEA0}, ia: []uint8{nas.NIA0, nas.NIA2}, wantErr: ErrUnsupportedAlgorithms},
		// WithSMCIntegrity keeps protecting with 128-NIA2, so a command
		// selecting 128-NIA1 carries a MAC that is wrong under 128-NIA1.
		{name: "MAC wrong under the selected algorithm", opts: []mockamf.Option{mockamf.WithSMCIntegrity(nas.NIA1)},
			ea: []uint8{nas.NEA0}, ia: []uint8{nas.NIA1, nas.NIA2}, wantErr: nas.ErrIntegrity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			ueCfg := bindTestUE(t, "208930100007500", ki, opc)
			if tc.ia != nil {
				caps, err := ue.NewSecurityCapability(tc.ea, tc.ia)
				if err != nil {
					t.Fatal(err)
				}
				ueCfg.SecurityCapabilities = &caps
			}
			sess, err := Attach(ctx, conn, cfg, ueCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if tc.wantErr == nil {
				if err != nil || sess.sec.IntegrityAlg != nas.NIA2 || sess.sec.CipheringAlg != nas.NEA0 {
					t.Fatalf("Attach = %v, %v; want 128-NIA2/NEA0", sess, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Attach err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// securityModeContext installs a context only when the command's algorithms
// are acceptable and its MAC verifies under them; otherwise s.sec stays nil.
func TestSecurityModeContextInstallsOnlyVerified(t *testing.T) {
	kAmf := make([]byte, 32)
	for i := range kAmf {
		kAmf[i] = byte(i)
	}
	both := mustCaps(t, []uint8{nas.NEA0, nas.NEA2}, []uint8{nas.NIA0, nas.NIA1, nas.NIA2})
	tests := []struct {
		name          string
		selIA, selEA  uint8 // what the command says
		macIA         uint8 // the algorithm its MAC is computed with
		withheld      bool
		wantErr       error
		wantInstalled bool
	}{
		{name: "NIA2 NEA2", selIA: nas.NIA2, selEA: nas.NEA2, macIA: nas.NIA2, wantInstalled: true},
		{name: "NIA1 NEA0", selIA: nas.NIA1, selEA: nas.NEA0, macIA: nas.NIA1, wantInstalled: true},
		{name: "NIA0", selIA: nas.NIA0, selEA: nas.NEA0, macIA: nas.NIA2, wantErr: ErrUnsupportedAlgorithms},
		{name: "NIA3 not advertised", selIA: nas.NIA3, selEA: nas.NEA0, macIA: nas.NIA3, wantErr: ErrUnsupportedAlgorithms},
		{name: "NEA1 not advertised", selIA: nas.NIA2, selEA: nas.NEA1, macIA: nas.NIA2, wantErr: ErrUnsupportedAlgorithms},
		{name: "MAC under another algorithm", selIA: nas.NIA1, selEA: nas.NEA0, macIA: nas.NIA2, wantErr: nas.ErrIntegrity},
		{name: "withheld Security Mode Complete", selIA: nas.NIA1, selEA: nas.NEA0, macIA: nas.NIA2, withheld: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kInt, kEnc, err := auth.DeriveNASKeys(kAmf, tc.macIA, tc.selEA)
			if err != nil {
				t.Fatal(err)
			}
			amfSec := &nas.SecurityContext{IntegrityAlg: tc.macIA, CipheringAlg: tc.selEA, KNASint: kInt, KNASenc: kEnc, Network: true}
			raw, err := amfSec.EncodeSecureRaw(smcPlain(t, tc.selIA, tc.selEA, both), nas.SecHdrIntegrityNewContext)
			if err != nil {
				t.Fatal(err)
			}
			s := &attachState{caps: both, vec: &auth.Vector{KAmf: kAmf}}
			if tc.withheld {
				s.ueCfg.Probe = &AttachProbe{Withhold: map[uint8]bool{f5nas.MsgTypeSecurityModeComplete: true}}
			}
			msg, err := s.securityModeContext(raw)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
				if s.sec != nil {
					t.Error("a context was installed for a refused command")
				}
				return
			}
			if err != nil || msg == nil || msg.SecurityModeCommand == nil {
				t.Fatalf("= %v, %v; want the command", msg, err)
			}
			if tc.wantInstalled != (s.sec != nil) {
				t.Fatalf("installed = %v, want %v", s.sec != nil, tc.wantInstalled)
			}
			if s.sec != nil && (s.sec.IntegrityAlg != tc.selIA || s.sec.CipheringAlg != tc.selEA) {
				t.Errorf("installed 128-NIA%d NEA%d, want 128-NIA%d NEA%d", s.sec.IntegrityAlg, s.sec.CipheringAlg, tc.selIA, tc.selEA)
			}
		})
	}
}

func mustCaps(t *testing.T, ea, ia []uint8) nasType.UESecurityCapability {
	t.Helper()
	c, err := ue.NewSecurityCapability(ea, ia)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// smcPlain is a plain Security Mode Command selecting ia and ea and
// replaying caps.
func smcPlain(t *testing.T, ia, ea uint8, caps nasType.UESecurityCapability) []byte {
	t.Helper()
	m := f5nas.NewMessage()
	m.GmmMessage = f5nas.NewGmmMessage()
	m.SecurityHeader = f5nas.SecurityHeader{ProtocolDiscriminator: nasMessage.Epd5GSMobilityManagementMessage, SecurityHeaderType: f5nas.SecurityHeaderTypePlainNas}
	m.GmmHeader.SetMessageType(f5nas.MsgTypeSecurityModeCommand)
	smc := nasMessage.NewSecurityModeCommand(f5nas.MsgTypeSecurityModeCommand)
	smc.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	smc.SetMessageType(f5nas.MsgTypeSecurityModeCommand)
	smc.SelectedNASSecurityAlgorithms.SetTypeOfIntegrityProtectionAlgorithm(ia)
	smc.SelectedNASSecurityAlgorithms.SetTypeOfCipheringAlgorithm(ea)
	smc.ReplayedUESecurityCapabilities.SetLen(caps.GetLen())
	copy(smc.ReplayedUESecurityCapabilities.Buffer, caps.Buffer)
	m.SecurityModeCommand = smc
	b, err := m.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Before any Security Mode Command, a protected downlink's header is read
// for its type only: the read installs no context and verifies nothing.
func TestPreSMCTypeOnlyRead(t *testing.T) {
	var kInt, kEnc [16]byte
	amfSec := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc, Network: true}
	accept, err := amfSec.EncodeSecureRaw([]byte{0x7e, 0x00, 0x42, 0x01, 0x01}, nas.SecHdrIntegrityCiphered)
	if err != nil {
		t.Fatal(err)
	}
	pdu := dlNASTransport(1, 1, accept)
	var got DownlinkEvent
	s := &attachState{ueCfg: UEConfig{Probe: &AttachProbe{OnDownlink: func(ev DownlinkEvent) { got = ev }}}}
	if err := s.observe(&pdu); err != nil {
		t.Fatal(err)
	}
	if got.NASType != f5nas.MsgTypeRegistrationAccept {
		t.Errorf("type = %#02x, want the Registration Accept's", got.NASType)
	}
	if s.sec != nil {
		t.Error("a pre-SMC read installed a context")
	}
	if !s.readableCipher() {
		t.Error("readableCipher = false before any SMC")
	}
	s.sec = &nas.SecurityContext{CipheringAlg: nas.NEA2}
	if s.readableCipher() {
		t.Error("readableCipher = true under NEA2")
	}
}

// dlNASTransport is a DOWNLINK NAS TRANSPORT carrying nasPDU.
func dlNASTransport(amfID, ranID int64, nasPDU []byte) ngapType.NGAPPDU {
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
	ies[2].Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
	m.ProtocolIEs.List = ies
	return pdu
}
