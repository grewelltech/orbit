# Real Application Traffic for loom + orbit — Final Unified Architecture

Status: accepted 2026-07-17. This is the canonical cross-repo design; the
loom-side subset lives in the [loom repo](https://github.com/bgrewell/loom)
docs, and the loom work items are tracked as issues there.

**VoIP-first (RTP/RTCP + ITU-T G.107 MOS), then HTTP/TLS and video over gVisor netstack, all protocol engines and the far-end responder in loom, orbit as thin consumer.**

Base: loom-purist single-seam architecture (`netpath.Network` + app registries + stock-loomd far end). Grafted in: standards-fidelity's measurement science (RFC 3550 Appendix-A discipline, Gilbert BurstR, owd.Tracker error bars, G.107 component breakdown), testbed-pragmatist's risk staging (additive demux before Manager migration, version-skew gate, ops details), and the per-gNB multi-address netstack shape. All judge-identified flaws are addressed inline and marked **[fix]**.

---

## 1. Component diagram

```
 RAN box (orbit serve, loom embedded as library)             N6 box (stock loomd)
┌──────────────────────────────────────────────────┐        ┌───────────────────────────────┐
│ orbit CLI ── Connect RPC ── engine.Manager       │ loom.v1│ loomd (agent, existing        │
│                   │                              │ control│  flowManager lifecycle)       │
│   ┌───────────────┴────────────┐                 │ gRPC   │ ┌───────────────────────────┐ │
│   │ appsession.Session         │◄────────────────┼─(mgmt──┼►│ app.Server "voip"         │ │
│   │  loom control client       │  Capabilities   │  net,  │ │  (RTP answerer, latch)    │ │
│   │  + owd.Tracker (TimeSync)  │  Configure/Start│  NOT   │ │ app.Server "http"         │ │
│   │  + remote StreamTelemetry  │  StreamTelemetry│ tunnel)│ │  (HTTPOrigin: objects,    │ │
│   └──────┬─────────────────────┘                 │        │ │   TLS/h2, HLS/DASH segs)  │ │
│          │ in-process loom library               │        │ └─────────────┬─────────────┘ │
│  ┌───────▼────────────────────────────────────┐  │        │   netpath "host" (kernel)     │
│  │ app.Client voip / httpx / vidstream        │  │        └───────────────┼───────────────┘
│  │     │ net.Conn / net.PacketConn            │  │                        │ N6 network
│  │ netpath.Network                            │  │                 ┌──────▼──────┐
│  │  ├ "dgram"    (UDP, real IP/UDP hdrs)      │  │    GTP-U N3     │  UPF        │
│  │  └ netstack.Stack.Network(ueIP)            │  │◄═══════════════►│ (N3)   (N6) │
│  │    (ONE gVisor stack per gNB, all UE addrs)│  │  one socket     └─────────────┘
│  │     │ datapath.Tx/RxDatapath {RawL3}       │  │  per gNB
│  │ orbit-gtp dp over UEFlow + Demux           │  │  (after Phase 5)
│  └────────────────────────────────────────────┘  │
│  N3 Demux: TEID → UE → {ICMP→latency,          │
│            port→dgram ring, default→netstack}   │
│  hub StateEvents (handover ts) ─► correlate.go ─► annotated timeline / Prometheus │
└──────────────────────────────────────────────────┘
```

Layering inside loom (new packages in **bold**):

```
app.Client / app.Server         core/app/{voip,httpx,vidstream}      ← protocol engines
   │ net.Conn / net.PacketConn
netpath.Network                 core/netpath {"host","dgram",memory} + core/netstack
   │ frames (raw L3)
datapath.Tx/RxDatapath          core/datapath (contract unchanged, +RawL3 cap)
measurement                     core/rtp, core/rtp/rtcp, core/rtp/codec,
                                core/quality/{emodel,gilbert}, core/owd, core/metrics
coordination                    control/ (agent app roles), controller/ (placement+telemetry)
```

One seam rule **[fix: no ad-hoc transports]**: there is exactly one connection-factory abstraction, `netpath.Network`. VoIP, HTTP, video, and the future SIP UA all dial/listen through it. No `media.Transport`, no separate emul `Dialer`/`Listener` funcs — `core/emul/reqresp` is refactored onto `netpath.Network` too, retiring its concrete `net.Dial`/`net.Listen` (the verified defect that made reqresp untunnelable).

---

## 2. loom public API additions (definitive, verbatim Go)

### 2.1 `core/netpath` — the socket-semantics seam (NEW)

```go
// Package netpath provides connection-oriented network access (net.Conn /
// net.PacketConn semantics) as an injectable component: kernel stack,
// UDP-over-datapath, gVisor-over-datapath, or in-memory test loopback.
package netpath

type Network interface {
    Name() string
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
    ListenPacket(network, address string) (net.PacketConn, error)
    Listen(network, address string) (net.Listener, error)
    Close() error
}

// Options are PURE DATA (registry-safe, ADR-0006 pattern). Embedders that
// construct datapaths out-of-band (orbit) do NOT go through the registry —
// they call the direct constructors below.  [fix: no live instances in Options]
type Options struct {
    Local        netip.Addr // source addr for datapath-backed networks; optional bind for "host"
    MTU          int
    TxDatapath   string
    RxDatapath   string
    DatapathOpts datapath.Options
}

func Host(local netip.Addr) Network                 // kernel stack, default
func Memory() (a, b Network)                        // paired in-memory nets for tests
```

```go
// Package dgram: UDP-only Network encoding real IPv4+UDP headers (checksums
// included) into frames of a raw-L3 datapath. Generalizes orbit's
// BuildUDPPacket into loom. Dial/Listen("tcp") return ErrTCPUnsupported.
package dgram // core/netpath/dgram

// New is the embedder constructor (live datapaths).
func New(tx datapath.TxDatapath, rx datapath.RxDatapath, local netip.Addr, mtu int) (netpath.Network, error)
// FromOptions is the registry factory (resolves names via Components).
func FromOptions(c *components.Components, o netpath.Options) (netpath.Network, error)
```

`core/datapath.Capabilities` gains one additive field:

```go
type Capabilities struct {
    RawL2              bool
    RawL3              bool   // NEW: frames are complete IP packets
    HardwareTimestamps bool
    MaxPPS             uint64
}
```

### 2.2 `core/netstack` — one gVisor stack per gNB **[fix: not per-UE]**

```go
// Package netstack wraps gvisor.dev/gvisor/pkg/tcpip (pinned release, pure Go,
// no NET_ADMIN/TUN/netns). ONE Stack hosts MANY local addresses (all UEs of a
// gNB); per-UE isolation comes from per-connection source binding.
package netstack

type Config struct {
    MTU               int    // inner-IP MTU; orbit passes 1400 (1500 − outer IP 20 − UDP 8 − GTP-U 8..16 − slack)
    CongestionControl string // "cubic" (default) | "reno"; SACK + RACK enabled
}

type Stack struct{ /* *stack.Stack + dpEndpoint */ }

func New(cfg Config, tx datapath.TxDatapath, rx datapath.RxDatapath) (*Stack, error) // tx/rx must advertise RawL3
func (s *Stack) AddAddress(a netip.Addr) error     // UE attach
func (s *Stack) RemoveAddress(a netip.Addr) error  // UE release
// Network returns a per-UE source-bound netpath.Network view: DialContext binds
// the given local address (gonet.DialTCPWithBind), Listen binds on it. Closing
// a view does not close the Stack.
func (s *Stack) Network(local netip.Addr) netpath.Network
func (s *Stack) Close() error
```

Integration: `dpEndpoint` implements `stack.LinkEndpoint` directly over loom's frame contract (no `channel.Endpoint`, avoiding one copy per packet): `WritePackets → TxReserve/copy/TxCommit`; one RX goroutine loops `RxPoll(64) → InjectInbound(ipv4/ipv6) → RxRelease`. Pure L3 endpoint (no link addr, `CapabilityNone`), matching GTP-U inner traffic. gVisor imports isolated in this one package; `loom_nonetstack` build tag stubs it for minimal agents.

### 2.3 `core/rtp` — RFC 3550/3551/7587, with the algorithm mandates **[fix: spec pinned]**

```go
package rtp

type Header struct {
    Padding, Extension, Marker bool
    PayloadType    uint8
    SequenceNumber uint16
    Timestamp, SSRC uint32
    CSRC           []uint32
}
func (h *Header) MarshalTo(b []byte) (int, error)                  // 12 + 4·len(CSRC)
func ParseHeader(b []byte) (h Header, payloadOffset int, err error)

// Packetizer: SSRC, initial seq and initial timestamp from crypto/rand
// (RFC 3550 §5.1). Timestamp advances by SamplesPerPacket on the MEDIA clock —
// never derived from time.Now() (wall-clock stamping makes receiver jitter
// measure the sender's scheduler). Marker set on first packet of a talkspurt.
type Packetizer struct{ /* codec, ssrc, seq, ts */ }
func NewPacketizer(c codec.Codec) *Packetizer
func (p *Packetizer) SSRC() uint32
func (p *Packetizer) Next(buf, payload []byte) (n int)

// PayloadSource: G.711 = band-limited synthetic speech (decodes/plays in
// Wireshark); Opus = valid TOC byte (20ms SILK-WB/CELT config) + pseudo-random
// body at the CBR target — "wire-format-true, content-synthetic", documented.
type PayloadSource interface{ Fill(buf []byte, pktIndex uint64) int }
func NewG711Source(law string) PayloadSource
func NewOpusSource(bitrateBps int) PayloadSource

// ReceiverStats implements RFC 3550 Appendix A EXACTLY (mandated in pkg docs,
// pinned by spec-vector tests):
//  A.1: 16→32-bit seq extension with cycle counting; MIN_SEQUENTIAL=2
//       probation; MAX_DROPOUT=3000; MAX_MISORDER=100; big jumps re-init_seq.
//  A.3: expected = ext_max − base + 1; lost signed, clamped to 24-bit
//       [−0x800000, 0x7FFFFF] only on the wire (negative under duplication).
//  Fraction lost: PER-INTERVAL, 8-bit fixed point, 0 if negative.
//  A.8: transit D in RTP TIMESTAMP UNITS; J += |D| − ((J+8)>>4) on 16× state;
//       exported raw (RR field) and ms (J/clockRate·1000); equal-ts packets excluded.
type ReceiverStats struct{ /* per A.1 source struct */ }
func NewReceiverStats(clockRate uint32) *ReceiverStats
func (s *ReceiverStats) Observe(h Header, payloadLen int, arrival time.Time)
func (s *ReceiverStats) Report() ReportBlockData   // feeds RTCP RR
func (s *ReceiverStats) Interval() RxSnapshot      // delta since last call
func (s *ReceiverStats) Cumulative() RxSnapshot

type RxSnapshot struct {
    Received, Duplicates, Reordered uint64
    Expected                        uint64
    CumulativeLost                  int64
    FractionLost                    float64
    ExtHighestSeq, JitterTicks      uint32
    JitterMs                        float64
    MaxGap                          time.Duration // longest interarrival gap (media-gap primitive)
    MediaGaps                       []Gap          // gap opens after >3·ptime silence
}
type Gap struct{ Start, End time.Time; PacketsLost uint32 }
```

```go
package codec // core/rtp/codec

type Codec struct {
    Name        string        // "pcmu","pcma","g729","opus"
    PayloadType uint8         // 0, 8, 18 static; opus dynamic (default 111)
    ClockRate   uint32        // 8000; opus ALWAYS 48000 (RFC 7587 §4.1)
    Channels    uint8
    Ptime       time.Duration // default 20ms
    PayloadBytes     func(ptime time.Duration) int     // pcmu@20ms=160; g729@20ms=20
    SamplesPerPacket func(ptime time.Duration) uint32  // opus@20ms = 960 (48kHz clock)
    FrameLookahead   time.Duration // codec algorithmic delay: g711 0.25ms, g729 15ms, opus 26.5ms
    Ie, Bpl          float64       // G.113 App. I (g711 Bpl PLC-dependent: 25.1 PLC on [default], 4.3 off)
    Wideband         bool          // → G.107.1 scoring, IeWB/BplWB fields used
    IeWB, BplWB      float64
}
func ByName(name string) (Codec, error)
func Register(c Codec)
```

### 2.4 `core/rtp/rtcp` — SR/RR/SDES/BYE + XR, timing per §6.3/A.7 **[fix: interval + NTP discipline]**

```go
package rtcp

type Packet interface{ AppendTo(b []byte) []byte }
type ReportBlock struct { SSRC uint32; FractionLost uint8; CumulativeLost int32 // 24-bit on wire
    ExtHighestSeq, Jitter, LSR, DLSR uint32 }
type SenderReport struct { SSRC uint32; NTPSec, NTPFrac, RTPTime uint32
    PacketCount, OctetCount uint32; Reports []ReportBlock }
type ReceiverReport struct { SSRC uint32; Reports []ReportBlock }
type SDES struct{ Chunks []SDESChunk }              // CNAME mandatory in every compound (§6.5)
type Bye struct{ SSRCs []uint32; Reason string }

// RFC 3611 XR blocks: BT=4 Receiver Reference Time, BT=5 DLRR (non-sender RTT),
// BT=7 VoIP Metrics (loss/discard/burst/gap densities Gmin=16, delays, R/MOS ×10).
type XRReceiverRefTime struct{ NTPSec, NTPFrac uint32 }
type XRDLRR struct{ Items []DLRRItem }
type XRVoIPMetrics struct { SSRC uint32
    LossRate, DiscardRate, BurstDensity, GapDensity uint8      // /256 fixed point
    BurstDuration, GapDuration, RoundTripDelay, EndSystemDelay uint16 // ms
    Gmin, RFactor, ExtRFactor, MOSLQ, MOSCQ uint8              // MOS ×10, 127 = unavailable
    JBNominal, JBMaximum, JBAbsMax uint16 }

func MarshalCompound(pkts ...Packet) ([]byte, error) // enforces SR/RR-first + SDES/CNAME
func ParseCompound(b []byte) ([]Packet, error)
func IsRTCP(b []byte) bool                            // RFC 5761 rtcp-mux classification

// NTP discipline (pinned by tests): 1900 epoch (Unix + 2208988800);
// frac = nanos·2^32/1e9; LSR = middle 32 bits of SR NTP; DLSR in 1/65536 s.
// RTT at sender = A − LSR − DLSR, ALL in 16.16 fixed point, converted last.
func NTPNow(t time.Time) (sec, frac uint32)
func RTTFromReport(arrival time.Time, rb ReportBlock) (time.Duration, bool)

// Interval implements RFC 3550 §6.3/A.7: Td = max(Tmin, n·C), randomized
// [0.5,1.5)·Td / 1.21828, 5% bandwidth share, 75/25 sender/receiver split,
// reconsideration. Prevents fleet-mode RTCP sync storms. Tmin default 5s.
type Interval struct{ SessionBW float64; Members, Senders int; WeSent, Initial bool; Tmin time.Duration }
func (iv *Interval) Next(rng *rand.Rand) time.Duration
```

SR's `RTPTime` maps the same sampling instant as its NTP timestamp (media clock anchored to wall clock at session start) — receivers use SR pairs for clock mapping.

### 2.5 `core/quality/gilbert` — burst-loss estimator **[fix: BurstR has an estimator]**

```go
package gilbert

// Estimator fits an online 2-state Markov (Gilbert) loss model from the
// extended-seq loss/receive run-lengths: p = P(loss|prev recv), q = P(recv|prev loss).
// BurstR = 1/(p+q); 1 = random, >1 = bursty. Also emits RFC 3611 burst/gap
// density and duration with Gmin=16. ONE estimator shared by XR VoIP-metrics
// emission and the E-model's Ie,eff.
type Estimator struct{ /* run-length state */ }
func New(gmin int) *Estimator
func (e *Estimator) Observe(lost bool, at time.Time)
func (e *Estimator) Metrics() Metrics
type Metrics struct {
    P, Q, BurstR float64 // BurstR clamped ≥ 1
    BurstDensity, GapDensity float64
    BurstDuration, GapDuration time.Duration
}
```

### 2.6 `core/quality/emodel` — ITU-T G.107 / G.107.1, auditable **[fix: Ta/Ppl semantics + breakdown]**

```go
package emodel

type Config struct {
    Codec    codec.Codec
    Wideband bool    // G.107.1 (R scale 0..129, own Idd coefficients + own R→MOS map)
    A        float64 // advantage factor, default 0 (do not hide impairment)
}
// Input semantics are EXPLICIT:
//   Ta  = network OWD + jitter-buffer nominal + codec frame+lookahead delay
//         (helper ComposeTa below; feeding raw OWD understates Id).
//   Ppl = percent (0..100) INCLUDING jitter-buffer discards (RFC 3611 discard
//         semantics) — this is what makes delay spikes hurt MOS.
type Input struct {
    Ta     time.Duration
    Ppl    float64
    BurstR float64 // from gilbert; clamped ≥ 1
}
type Components struct{ Ro, Is, Idte, Idle, Idd, Id, Ie, IeEff, A, R float64 } // audit breakdown
type Result struct{ R, MOSCQ float64; Method string /* "g107"|"g107.1" */; C Components }

func Score(cfg Config, in Input) (Result, error)
func ComposeTa(networkOWD, jbNominal time.Duration, c codec.Codec) time.Duration
func IeEff(ie, bpl, ppl, burstR float64) float64 // Ie + (95−Ie)·Ppl/(Ppl/BurstR + Bpl), Ppl in PERCENT
func MOSFromR(r float64) float64                  // G.107 Annex B; 1 below 0, 4.5 above 100
func MOSFromRWB(r float64) float64                // G.107.1 mapping on the 0..129 scale (NOT the NB polynomial)
```

Mandated math (in package docs + golden tests): full default formulas so zero-impairment R = 93.2 ± 0.01 (Ro ≈ 94.77, Is ≈ 1.41 computed, not constants); `Id = Idte + Idle + Idd` with `T = Ta`, `Tr = 2·Ta` (symmetric-path default; Ta/T/Tr never conflated); **Idd = 0 for Ta ≤ 100 ms**, else `X = log10(Ta/100)/log10 2`, `Idd = 25·[(1+X⁶)^(1/6) − 3·(1+(X/3)⁶)^(1/6) + 2]` — no FiDO2011 curve fit anywhere. Golden tests pin the G.107 Table 4 verification examples and the R→MOS reference table. Opus rows are provisional non-ITU values, documented as such with `codec.Register` override.

### 2.7 `core/owd` — one-way delay with honest error bars **[fix: drift + ErrBound]**

```go
package owd

type Method int
const ( Synced Method = iota; RTTHalf; AssumeSynced )

type Estimate struct {
    Value    time.Duration
    ErrBound time.Duration // half-width: min-filtered sync delay/2 + drift residual
    Method   Method
    Valid    bool
}
type OffsetProvider interface {
    Offset() (offset, errBound time.Duration, ok bool) // remote − local
}

// Tracker turns repeated four-timestamp exchanges (core/timesync.Sample) into
// a filtered offset with drift: minimum-delay sample per window (NTP
// clock-filter style), linear offset(t) fit over the last N windows,
// ErrBound = residual + delay/2. Fed by whoever runs the exchanges (orbit's
// engine or the loom controller loop) over a SYMMETRIC path (mgmt network,
// never through the tunnel).
type Tracker struct{ /* windows, fit */ }
func NewTracker(window time.Duration, n int) *Tracker
func (t *Tracker) Feed(s timesync.Sample, at time.Time)
func (t *Tracker) Offset() (offset, errBound time.Duration, ok bool) // satisfies OffsetProvider
```

### 2.8 `core/app` — app framework + registries (NEW roles, ADR-justified)

```go
package app

type Options struct {
    Params  map[string]string   // codec, ptime, jb_ms, objects, ladder, port_min/port_max…
    Seed    int64
    MTU     int
    Network netpath.Network     // resolved by agent (registry) or embedder (direct)
    Target  string              // client side: server host:port
    OWD     owd.OffsetProvider  // nil ⇒ RTT/2 fallback, labeled
}

// Client and Server are flow.Runners: the agent's existing flowManager
// lifecycle (Configure/Arm/Start/Stop/Destroy, panic containment, telemetry
// boundaries) applies unchanged.
type Client interface {
    Name() string
    Run(ctx context.Context) error
    Counters() *accounting.Counters
}
type Server interface {
    Name() string
    Run(ctx context.Context) error
    Counters() *accounting.Counters
    Addr() netip.AddrPort // bound addr → Configure's data_port (Receiver.Port() pattern)
}
```

`core/components.Components` gains three additive registries:

```go
type Components struct {
    // …existing five…
    Networks   *registry.Registry[netpath.Network, netpath.Options]
    AppClients *registry.Registry[app.Client, app.Options]
    AppServers *registry.Registry[app.Server, app.Options]
}
```

**Role decision + ADR [fix: justify vs RESPONDER reuse].** New `FLOW_ROLE_APP_CLIENT=6` / `FLOW_ROLE_APP_SERVER=7` rather than overloading `RESPONDER` with an emulation-name selector. Rationale recorded in an ADR: (a) `core/emul` is documented and implemented as *shape-only* carriage (mode.go); housing a wire-true protocol engine under an emulation name blurs loom's own taxonomy; (b) RESPONDER/REQUESTER semantics are coupled to the reqresp transport field and BehaviorScript; apps are bidirectional, have their own metrics plane, and dispatch on `FlowSpec.app`; (c) the reflector's `Unimplemented` arm stays untouched. The alternative (selector on RESPONDER) is documented as considered-and-rejected in the ADR so the loom maintainer sees the reasoning.

### 2.9 `core/app/voip` — the VoIP pair (SDP-shaped SIP seam)

```go
package voip

// MediaConfig is exactly what SDP offer/answer will produce — the SIP seam.
// The future "sip" app negotiates and then hands this struct to NewMediaSession.
type MediaConfig struct {
    Codec          codec.Codec
    LocalRTP       netip.AddrPort // 0 port ⇒ ephemeral even port; RTCP-mux (RFC 5761) default on
    RemoteRTP      netip.AddrPort // zero ⇒ answerer mode: latch first valid source
    SSRC           uint32         // 0 = crypto/rand
    Direction      Direction      // SendRecv | SendOnly | RecvOnly
    JitterBufferMs int            // fixed playout model, default 40; late arrivals = discards → Ppl
    HandshakeTimeout time.Duration // default 5s; see latch rules below
}

type MediaSession struct{ /* tx pace @Ptime, rx loop, rtcp.Interval loop, gilbert, owd */ }
func NewMediaSession(n netpath.Network, cfg MediaConfig, o owd.OffsetProvider) (*MediaSession, error)
func (m *MediaSession) Run(ctx context.Context) error
func (m *MediaSession) Metrics() metrics.VoIP     // both directions + remote XR view

func NewClient(o app.Options) (app.Client, error) // registered "voip"
func NewServer(o app.Options) (app.Server, error) // registered "voip" (answerer)
```

**Rendezvous latch rules [fix: crisp semantics].** Answerer (server): binds inside `port_min..port_max` when given (firewall determinism); latches the first `(srcAddr, SSRC)` pair whose packets pass RTP validity + A.1 probation (2 in-order packets); all other sources are dropped and counted (`stray_packets`). Caller (client): starts media immediately; if no return RTP or RTCP arrives within `HandshakeTimeout`, `Run` returns a typed handshake error (surfaced through telemetry). loom's auth token gates who may Configure the server; far-end flows are always duration-bounded (orphan protection). SIP replaces the latch with explicit SDP addresses later — media engine untouched.

### 2.10 `core/app/httpx` and `core/app/vidstream` **[fix: far end is loom, not nginx]**

```go
package httpx // registered as "http"

// Client: real HTTP/1.1 + TLS (crypto/tls) + optional h2 (x/net/http2) via an
// http.Transport whose DialContext is the injected netpath.Network — the whole
// stdlib client stack rides the datapath. Per-request timings: connect, TLS
// handshake, TTFB, transfer; aggregates p50/p95/p99.
func NewClient(o app.Options) (app.Client, error) // params: url_path, objects, object_size, think, tls, h2, host
// Server ("HTTPOrigin"): net/http on the Network's Listener. Endpoints:
//   GET /object/{bytes}                          deterministic bodies
//   GET /media/{name}/manifest.m3u8|.mpd         generated ladder manifest
//   GET /media/{name}/{kbps}/seg{n}              segment sized kbps·segdur/8
// Self-signed TLS on demand; h2. This is the loom-owned far end per locked
// decision 1; nginx remains only an optional realism cross-check.
func NewServer(o app.Options) (app.Server, error)
```

```go
package vidstream // registered as "video"; client-only (server is httpx)

// ABR player buffer model over httpx: fetch manifest, then segments; virtual
// playhead drains buffer in real time; stall = buffer 0 while playing; resume
// at rebuffer_target; throughput- or buffer-based ABR.
func NewClient(o app.Options) (app.Client, error) // params: ladder, seg_duration, buffer_target, abr
```

### 2.11 `core/metrics` — results plane

```go
package metrics

type Source interface{ Metrics() Snapshot }   // agent type-asserts at telemetry
type Snapshot interface{ Kind() string }      // boundaries, same pattern as flowTCPInfo

type VoIP struct {
    Codec string
    TxPackets, RxPackets, Lost, Duplicates, Reordered uint64
    LossPct, DiscardPct float64      // network loss vs jitter-buffer discard, both feeding Ppl
    JitterMs, RTTMs float64
    OWDMs, OWDErrMs float64          // error bar carried everywhere [fix]
    OWDMethod string                 // "timesync" | "rtt/2" | "assume-synced" | "none"
    BurstR, RFactor, MOSCQ float64
    EModel emodel.Components         // Ro/Is/Idte/Idle/Idd/Ie,eff audit breakdown [fix]
    RemoteRFactor, RemoteMOSCQ float64 // peer's view via RTCP XR
    MediaGaps []rtp.Gap
}
type HTTP struct {
    Requests, Errors uint64
    ConnectMs, TLSHandshakeMs, TTFBMsP50, TTFBMsP95, ObjectMsP95, GoodputMbps float64
}
type Video struct {
    SegmentsFetched, Stalls uint64
    StartupMs, StallTimeMs, RebufferRatio, BufferMs, AvgBitrateKbps float64
    RepSwitchesUp, RepSwitchesDown uint64
    StallEvents []rtp.Gap
}
```

### 2.12 `loom.v1` wire changes (all additive; field numbers verified free against control.proto)

```proto
enum FlowRole {
  // existing 0..5 unchanged (REFLECTOR=3 stays Unimplemented)
  FLOW_ROLE_APP_CLIENT = 6;
  FLOW_ROLE_APP_SERVER = 7;
}
message FlowSpec {
  // existing 1..15, role = 21 unchanged
  string app     = 16; // "voip" | "http" | "video"
  string network = 17; // netpath network name; "" = "host"
  string local   = 18; // local addr for datapath-backed networks
}
message TelemetrySample {
  // existing 1..11 unchanged (final = 10, tcp = 11 — verified)
  AppMetrics app = 12;
}
message AppMetrics { oneof kind { VoipMetrics voip = 1; HttpMetrics http = 2; VideoMetrics video = 3; } }
message VoipMetrics { double mos_cq = 1; double r_factor = 2; double jitter_ms = 3;
  double loss_pct = 4; double discard_pct = 5; double burst_r = 6;
  double rtt_ms = 7; double owd_ms = 8; double owd_err_ms = 9; string owd_method = 10;
  uint64 rx_packets = 11; uint64 lost = 12; repeated MediaGap gaps = 13;
  double remote_mos_cq = 14; EModelBreakdown emodel = 15; }
message EModelBreakdown { double ro = 1; double is = 2; double idte = 3; double idle = 4;
  double idd = 5; double ie_eff = 6; }
message MediaGap { int64 start_unix_nanos = 1; int64 end_unix_nanos = 2; uint32 packets_lost = 3; }
message HttpMetrics { uint64 requests = 1; uint64 errors = 2; double ttfb_ms_p95 = 3;
  double goodput_mbps = 4; double tls_handshake_ms = 5; double connect_ms = 6; }
message VideoMetrics { uint64 stalls = 1; double stall_time_ms = 2; double rebuffer_ratio = 3;
  double buffer_ms = 4; double avg_bitrate_kbps = 5; double startup_ms = 6;
  repeated MediaGap stall_events = 7; }
message CapabilitiesResponse { /* existing 1..5 */ repeated string networks = 6; repeated string apps = 7; }
```

**Version-skew gate [fix].** Every consumer (orbit, loom controller) checks `CapabilitiesResponse.apps`/`networks` at provision time and fails fast with an actionable error: `loomd at n6:9551 (v0.9.1) lacks app "voip"; run loom >= v0.10`. All changes additive per ADR-0021, so mixed versions degrade to clean refusals.

`control/agent.go`: `configureAppServer` (build Network from `Components.Networks` per `FlowSpec.network`, `AppServers.Build(app, opts)`, return `Addr().Port()` as `data_port`) and `configureAppClient`; the telemetry streamer type-asserts `metrics.Source` at boundaries exactly like `flowTCPInfo`. `controller/`: scenario `flow: {kind: voip|http|video}` → APP_SERVER on `to` + APP_CLIENT on `from`; `foldLocked` carries AppMetrics into FlowSample/Aggregate; Text/JSON observers render MOS/QoE lines. Quick mode: **`loom rtp --answer` / `loom rtp --call host:port --codec g711 --duration 60s`** — the standalone non-orbit proof point (Phase 3 demo runs with zero orbit involvement).

---

## 3. Responder role design (the N6 side)

The far end is a **stock loomd agent** — no new daemon, no orbit code on N6. loom already provides lifecycle, auth (ADR-0014), TimeSync, and boundary-anchored telemetry. `Configure(role=APP_SERVER, app="voip"|"http", network="host", params{port_min,port_max,codec,…})` → server built and registered under `flowManager`, `data_port` returned, flow duration-bounded (orphan protection even if orbit crashes). Both ends stream telemetry: orbit subscribes `StreamTelemetry` on the N6 loomd for the server-side (uplink-as-received) series while the in-process client supplies the downlink series. VoIP answerer latches per §2.9; HTTP/video far end is loom's `httpx` HTTPOrigin (objects + generated HLS/DASH ladder + TLS/h2) — locked decision 1 honored; nginx only as optional cross-validation. SIP seam: `voip.MediaConfig` is the SDP-negotiable set; the "sip" app (next step) drives INVITE/SDP over the same `netpath.Network` and hands the negotiated MediaConfig to `NewMediaSession`.

## 4. RTP/RTCP + G.107 measurement pipeline

```
UE-side client (orbit, dgram over GTP-U)                  N6-side server (loomd, host net)
 Packetizer @ Ptime (media-clock ts, crypto/rand ids) ──► ReceiverStats.Observe(hdr, rxTime)
 ReceiverStats.Observe ◄── downlink media ────────────── Packetizer (bidirectional call)
   │ per packet: A.1 ext-seq/probation → loss/dup/reorder; A.8 jitter (ts units);
   │ gilbert.Observe(lost) → p,q,BurstR + burst/gap densities; JB model → discards
   ├ RTT: RFC 3550 §6.4.1 A − LSR − DLSR (16.16 fixed point)   ◄── RTCP SR/RR/XR ──┐
   ├ OWD: arrival_local − (SR NTP send time + owd.Tracker offset), ± ErrBound       │
   └ per telemetry boundary:                                                        │
       Ppl = network loss% + JB discard%   BurstR from gilbert                      │
       Ta  = ComposeTa(OWD, JB nominal, codec frame+lookahead)                      │
       emodel.Score → {R, MOS-CQ, Components breakdown} → metrics.VoIP ─► TelemetrySample.app
 RTCP tx cadence: rtcp.Interval (§6.3/A.7 randomized); XR VoIP-metrics carries R/MOS on the wire
```

Wire-realism acceptance test: Wireshark decodes RTP (correct PT/SSRC/seq/ts cadence, G.711 audio playable) and RTCP SR/RR/SDES/XR compounds; RTP stream analysis jitter/loss must match loom's own numbers.

## 5. Clock handling / one-way delay (definitive)

Three tiers, every OWD/Ta-derived number labeled with `Method` + `ErrBound` end-to-end (proto, CLI, Prometheus):
1. **timesync (default under orbit):** orbit runs loomd's existing `TimeSync` RPC every ~10 s **over the management network, never through the tunnel** (asymmetry would poison the offset); `owd.Tracker` min-delay-filters and drift-fits; sub-ms ErrBound on the testbed LAN — ample for Id (knee at 100 ms).
2. **rtt/2 fallback:** no control channel ⇒ OWD ≈ RTT/2 from LSR/DLSR with ErrBound = RTT/2 ("could be anywhere"), never silently presented as measured.
3. **assume-synced:** operator asserts NTP/PTP with a declared max error.
When ErrBound exceeds a threshold, the E-model input clamps to the RTT/2 tier and says so. Hardware timestamps slot into `Frame.Meta` later (ADR-0010/0020) with no API change.

## 6. N3 demux ownership model + netstack integration (orbit) **[fix: one model, no collisions]**

**Invariant: at any time, exactly one reader owns a given N3 socket, and consumers register with its Demux — never read the socket directly.**

```go
// internal/datapath (orbit)
type Demux struct{ /* single reader goroutine on one N3 conn */ }
func NewDemux(conn *net.UDPConn) *Demux            // wraps EITHER a legacy Tunnel's conn (Phase 4)
                                                    // OR the SharedTunnel conn (Phase 5+)
func (d *Demux) Register(dlTEID uint32) *UERx       // per-UE lane
func (d *Demux) Rebind(oldTEID, newTEID uint32) error // atomic handover TEID swap
type UERx struct{ /* dispatch table over inner IP */ }
func (u *UERx) SubscribeICMP() *Ring                 // latency probe
func (u *UERx) SubscribeUDP(dstPort uint16) *Ring    // dgram RxDatapath lanes (media)
func (u *UERx) SetDefaultSink(f func(innerIP []byte)) // netstack InjectInbound (Phase 6)
type Ring struct{ /* bounded, drop-oldest + counter (loom ADR-0005 spirit); arrival ts preserved */ }
```

Staging **[fix: no big-bang migration]**: Phase 4 layers `Demux` on the *existing per-session Tunnel socket* (additive; no new 2152 bind, so no EADDRINUSE collision with legacy sessions); the ICMP latency probe moves onto `SubscribeICMP` at the same time (today's `ReadDownlink` is single-consumer, so media and probe must share via the demux from day one). Phase 5 migrates `Session.tunnel()` to one `SharedTunnel` per gNB N3 address — from that point SharedTunnel's conn is the **only** 2152 bind per gNB (legacy Tunnel retired at cutover, not run in parallel) — gated on the existing `multisession`/`handover_data`/`pathswitch_data` integration suites passing on the shared path. End Marker G-PDUs are surfaced as correlation events.

`internal/loomgtp` becomes a full `datapath.TxDatapath`+`RxDatapath` pair (`Caps{RawL3:true}`) over `UEFlow` + `UERx`. `NetworkFor(sess)`: VoIP/UDP apps get `dgram.New` (lightweight — fleet VoIP never pays gVisor cost); TCP apps get the per-gNB `netstack.Stack` (created lazily, UE addrs added/removed on attach/release) via `Stack.Network(ueIP)`. Handover: `Demux.Rebind` + UEFlow TEID swap; the netstack never notices — TCP sees only delay/loss, which is the realism we want.

## 7. orbit integration: RPC/CLI surface + event correlation

`internal/engine/appsession.go` — controller-lite, embedding loom's own coordination machinery:

```go
func (m *Manager) StartAppSession(ctx context.Context, supi string, cfg AppSessionConfig) (id string, err error)
// AppSessionConfig{App, PeerAgent string; Params map[string]string; Duration time.Duration}
//  1. control.Dial(PeerAgent, token) → Capabilities gate (apps/networks; actionable error on skew)
//  2. TimeSync loop → owd.Tracker (mgmt path)
//  3. Configure APP_SERVER{app, network:"host", params incl. port_min/max} → data_port; Start
//  4. loomgtp.NetworkFor(sess) → app.Options{Network, Target: peerN6IP:data_port, OWD: tracker}
//  5. AppClients.Build → run as managed session; subscribe remote StreamTelemetry
func (m *Manager) AppSessionEvents(id string) (<-chan AppSample, func())
func (m *Manager) StopAppSession(ctx context.Context, id string) (AppSessionReport, error)
```

`proto/orbit/v1/ue.proto` (new RPCs): `StartApp`, `AppStream` (server-streaming `AppSample{unix_nano, local VoipMetrics/…, remote …, events[]}`), `StopApp` → `AppReport` with annotations, and `DataStats` exposing the existing per-QFI `Tunnel.Stats` (cheap Phase-0 win). CLI: `orbit ue app voip --supi … --peer n6:9551 --codec g711 --duration 60s [--json]`, `orbit ue app http|video …`, `orbit ue stats`.

**Event correlation** (`internal/engine/correlate.go`): orbit is the single clock and join point. `hub` StateEvents gain explicit phases (`HandoverStarted/HandoverComplete/PathSwitchComplete/EndMarkerReceived`). Remote (N6) samples are re-stamped onto orbit's clock via the `owd.Tracker` offset **with ErrBound propagated onto each re-stamped timestamp** [fix]; annotations therefore carry honest uncertainty: `XnHandover @t0 → DL media gap 240ms [±1ms] → MOS-CQ 4.2→2.1 (interval t0..t0+1s) → recovered @t0+3s`. Prometheus (existing `internal/observability`): `orbit_app_mos{supi,app,end="ue|n6"}`, `orbit_app_jitter_ms`, `orbit_app_loss_pct`, `orbit_app_owd_ms` + `orbit_app_owd_err_ms`, `orbit_app_ttfb_ms`, `orbit_app_stalls_total`, `orbit_app_media_gap_ms`, `orbit_ue_handover_timestamp_seconds` — Grafana annotations come free.

## 8. Deployment story

- **RAN box:** `orbit serve` unchanged (single binary, loom embedded, no new privileges — UDP sockets only; netstack is pure userspace). New flags `--loom-agent host:port --loom-token` so `--peer` can be omitted.
- **N6 box:** stock `loomd` from loom releases (existing `scripts/install.sh` + `loomd.service`, `upgrade.sh` for updates), `loomd --token $TOKEN`. Nothing orbit-specific; independently useful; upgraded on loom's cadence.
- **Firewall matrix (documented in docs/USAGE.md):** control 9551/tcp from RAN mgmt net; RTP `port_min..port_max` (e.g. 40000–40100/udp) from the UPF N6 subnet; HTTP data_port range likewise.
- **Fleet mode:** behaviors gain `traffic: {app: voip|http|video, …}` cohorts; one control connection + one TimeSync per N6 loomd shared across all UEs; per-gNB netstack Stack (multi-addr) for TCP cohorts; per-cohort aggregate MOS/QoE distributions (p5/p50/p95) in FleetReport + Prometheus. RTCP §6.3/A.7 randomization prevents sync storms at thousands of calls (~2 goroutines / 50 pps per call).
- **Single-node demo:** the README fleet-test setup works identically with loomd on the UPF's N6 network.

## 9. Netstack measurement hygiene **[fix: quantify, don't attribute]**

Before any TCP-derived number is claimed: (a) publish a netstack-vs-kernel benchmark delta (loopback + testbed) as a Phase-6 gate; (b) sender-side timestamp audit — compare media/send-clock intended departure vs actual `TxCommit` time so userspace-stack scheduling jitter is quantified and reported separately, never silently attributed to the RAN.

## 10. Loom issues to file

1. **`core/netpath`: injectable connection-factory seam (Network) + host/memory implementations** — Add `netpath.Network` (Dial/ListenPacket/Listen) as a registry component (`Components.Networks`); pure-data `Options`; `Host()`/`Memory()` impls. ADR: this seam supersedes concrete `net.Dial`/`net.Listen` in `core/emul/reqresp` (which today cannot ride any injected datapath). Includes refactoring reqresp onto the seam with back-compat wrappers.
2. **`core/datapath`: add `Capabilities.RawL3`** — One additive bool marking frame payloads as complete IP packets, so L3-consuming components (dgram, netstack) can validate backends. Update built-in datapaths accordingly.
3. **`core/netpath/dgram`: UDP-with-real-headers Network over raw-L3 datapaths** — IPv4+UDP header/checksum encode-decode per packet; embedder constructor (live tx/rx) + registry factory; ErrTCPUnsupported for tcp. Contract tests over the memory datapath.
4. **`core/rtp`: RFC 3550/3551 RTP — header codec, Packetizer, ReceiverStats** — Mandate A.1 probation/MAX_DROPOUT/MAX_MISORDER/cycle counting, A.3 signed 24-bit loss clamp, A.8 fixed-point jitter in timestamp units, media-clock timestamps (never wall clock), crypto/rand SSRC/seq/ts. Ship spec-vector tests + golden pcap that decodes in Wireshark. Include the "naive implementations" checklist in package docs.
5. **`core/rtp/codec`: codec table (PCMU/PCMA/G.729/Opus)** — PT/clock/ptime/payload sizes (Opus RTP clock always 48 kHz per RFC 7587; 960 samples @20 ms), frame+lookahead delays, G.113 Ie/Bpl (G.711 Bpl PLC-dependent), G.107.1 wideband rows (Opus provisional, documented), `Register` override. G.711 speech-plausible payload synthesis; Opus valid-TOC synthetic frames.
6. **`core/rtp/rtcp`: SR/RR/SDES/BYE + RFC 3611 XR (BT4/5/7), compound rules, §6.3/A.7 interval** — NTP 1900-epoch/middle-32/DLSR-1/65536s discipline; `RTTFromReport` in 16.16 fixed point; CNAME-mandatory compound enforcement; randomized interval scheduler with reconsideration. RFC 5761 rtcp-mux classification.
7. **`core/quality/gilbert`: online 2-state Markov loss estimator** — p, q, BurstR = 1/(p+q), RFC 3611 burst/gap densities/durations (Gmin=16); one estimator shared by XR emission and the E-model.
8. **`core/quality/emodel`: ITU-T G.107 + G.107.1 E-model** — Full-default formulas (zero-impairment R = 93.2 ± 0.01 test), Idd with 100 ms knee, Ie,eff with BurstR, Annex-B and G.107.1 R→MOS maps, `Components` audit breakdown, explicit `ComposeTa` (OWD + JB nominal + codec delay) and Ppl-includes-discards semantics. Golden tests pinned to G.107 Table 4 verification examples. Explicitly not a curve fit — record in DECISIONS.md.
9. **`core/owd`: OffsetProvider + Tracker with drift and error bars** — Min-delay filtering over `timesync.Sample` windows, linear drift fit, `Estimate{Value, ErrBound, Method}`; methods timesync/rtt-2/assume-synced always labeled.
10. **`core/app`: application framework (Client/Server, Options, registries) + new flow roles** — `AppClients`/`AppServers` registries; proto `FLOW_ROLE_APP_CLIENT=6`/`APP_SERVER=7`, `FlowSpec.app/network/local` (16–18), `TelemetrySample.app=12` + AppMetrics messages, `CapabilitiesResponse.networks=6/apps=7`; agent `configureAppServer/Client`; telemetry attaches `metrics.Source` snapshots (flowTCPInfo pattern). ADR documenting why new roles vs RESPONDER selector.
11. **`core/app/voip`: bidirectional RTP/RTCP media session with G.107 scoring** — MediaSession (Ptime pacing, jitter-buffer discard model, gilbert+emodel per boundary, XR emission), SDP-shaped MediaConfig as the SIP seam, answerer latch rules (probation, SSRC+addr lock, stray counting, bounded handshake timeout), `port_min/port_max` binding.
12. **`core/metrics`: results plane (VoIP/HTTP/Video snapshots)** — Source/Snapshot interfaces, snapshots incl. OWD error bars and E-model breakdown; controller `foldLocked` + observers render MOS/QoE; scenario kinds voip/http/video → APP placement.
13. **`cmd/loom`: `loom rtp --call/--answer` quick mode** — Standalone two-host VoIP demo with live interval MOS/jitter/loss/RTT/OWD(method±err) from both ends; the non-orbit proof point and dogfood path.
14. **`core/netstack`: gVisor tcpip as a netpath backend (one multi-address Stack)** — `stack.LinkEndpoint` implemented directly over the TxReserve/TxCommit + RxPoll/RxRelease frame contract (no channel.Endpoint copy); AddAddress/RemoveAddress; `Network(local)` source-bound views; MTU/CC config (SACK/RACK/cubic); pinned gVisor release isolated in one package; `loom_nonetstack` build tag; netstack-vs-kernel benchmark + sender timestamp audit harness.
15. **`core/app/httpx`: real HTTP/1.1 + TLS (+h2) client and HTTPOrigin server** — Client over injected DialContext with connect/TLS/TTFB/transfer timings; server with deterministic bodies, self-signed TLS, h2, and generated HLS/DASH manifest+segment endpoints (the loom-owned video far end).
16. **`core/app/vidstream`: ABR player model with QoE metrics** — Buffer model, stall/rebuffer/startup, ladder switching, throughput+buffer ABR policies over httpx; VideoMetrics with stall events for correlation.
17. **Docs/ADRs: netpath seam, app roles, gVisor dependency, metrics plane, E-model provenance, OWD methodology** — mkdocs pages (netpath, apps, quality scoring, clock-sync ladder) + ADR entries; DESIGN.md §10 protocol-tier table update; note that `core/emul` shapes remain shape-only by design.

## 11. Critical Files for Implementation

- /home/ben/repos/bgrewell/loom/core/components/components.go (Networks/AppClients/AppServers registries — the seam everything hangs off)
- /home/ben/repos/bgrewell/loom/control/agent.go (APP_CLIENT/APP_SERVER configure arms + metrics-attached telemetry streaming)
- /home/ben/repos/bgrewell/loom/proto/loom/v1/control.proto (FlowRole 6/7, FlowSpec 16–18, TelemetrySample.app=12, CapabilitiesResponse 6/7 — all verified free)
- /home/ben/repos/bgrewell/orbit/internal/loomgtp/loomgtp.go (Tx+Rx RawL3 datapath pair + NetworkFor bridge)
- /home/ben/repos/bgrewell/orbit/internal/datapath/shared.go (+ new demux: TEID → UE → protocol/port dispatch; the multi-UE prerequisite)

---

## 12. Phased roadmap (each phase independently demoable)

**Status — all phases 0–7 SHIPPED.** Phase 0 `8dbd898` (DataStats + handover
phase events); phases 1–3 in loom (tag v0.10 / v0.11); Phase 4 `86fa1f9` (VoIP
over GTP-U, app sessions, N3 demux, correlation); Phase 5 `e7c2f32` (per-gNB
`SharedTunnel` — one N3 socket per gNB, the multi-UE cutover); phases 6–7
`77966b4` / PR #35 (real HTTP/TLS + video via the per-gNB gVisor netstack, and
fleet app cohorts — `behaviors.traffic.mix` entries taking `app: voip|http|video`
with per-cohort p5/p50/p95 aggregates). Single-UE entry points are
`orbit ue app voip|http|video`; the fleet entry point is a cohort in a fleet
scenario. SIP remains queued, as noted in Phase 7.

Rationale for stating this here: the per-phase deliverables below read as
forward-looking plans, and without a status line they invite the conclusion that
the work is outstanding.

### Phase 0 — Cheap observability wins (orbit, one small PR)

- **Deliverable:** DataStats RPC + 'orbit ue stats' exposing the existing per-QFI Tunnel.Stats; explicit timestamped handover-phase StateEvents in hub (HandoverStarted/Complete/PathSwitchComplete). No data-path changes.
- **Testbed demo:** During a normal 'ue traffic' run on the testbed, 'orbit ue stats' shows live per-QFI UL/DL counters; an Xn handover emits cleanly timestamped phase events visible in serve logs — the correlation timeline's raw material exists.

### Phase 1 — loom measurement core (loom-only, 2-3 PRs)

- **Deliverable:** core/rtp (A.1/A.3/A.8-exact ReceiverStats, Packetizer, payload sources), core/rtp/codec, core/rtp/rtcp (SR/RR/SDES/BYE/XR + §6.3/A.7 interval + NTP discipline), core/quality/gilbert, core/quality/emodel (G.107+G.107.1 with Components breakdown), core/owd Tracker — all pure packages with spec-vector and G.107 Table-4 golden tests.
- **Testbed demo:** loom repo only: 'go test ./core/rtp/... ./core/quality/... ./core/owd/...' green; a generated pcap decodes fully in Wireshark (RTP stream analysis jitter/loss matches loom's numbers; G.711 audio plays); emodel example reproduces ITU reference R/MOS values incl. R=93.2 defaults.

### Phase 2 — loom netpath seam + voip app as a library (loom-only, 2 PRs)

- **Deliverable:** core/netpath (Network, host, memory, Networks registry, reqresp refactor ADR), core/netpath/dgram + Capabilities.RawL3, core/app (Client/Server/Options + AppClients/AppServers registries), core/app/voip MediaSession (latch rules, JB discard model, boundary MOS), core/metrics snapshots.
- **Testbed demo:** A Go example in the loom repo runs a bidirectional G.711 call between two processes over the host network: live interval jitter/loss/RTT/MOS both directions; tc netem loss moves MOS per the G.113 curve, bursty vs random loss scored differently via BurstR; the same session runs over the memory network in CI.

### Phase 3 — loom agent/controller wiring + quick-mode CLI (loom-only, 1-2 PRs; tag v0.10)

- **Deliverable:** proto additions (APP_CLIENT/APP_SERVER roles, FlowSpec 16-18, TelemetrySample.app=12, CapabilitiesResponse networks/apps), agent configureAppServer/Client + metrics-attached telemetry, controller scenario kind 'voip' + observers, 'loom rtp --call/--answer' quick mode, version-skew gate, port_min/max, duration-bounded flows.
- **Testbed demo:** Zero orbit involvement: stock loomd on the N6 box, 'loom rtp --call n6:9551' from the RAN box over the LAN — a 60s G.711 call with per-interval MOS/jitter/loss/RTT/OWD(method±err) from BOTH ends; Wireshark decodes RTP + RTCP SR/RR/XR; an old loomd is refused with 'lacks app voip; run loom >= v0.10'.

### Phase 4 — orbit VoIP over GTP-U + event correlation (single-UE, additive demux)

- **Deliverable:** Demux layered on the existing per-session Tunnel socket (ICMP latency probe migrated onto it, media UDP-port lanes added — no new 2152 bind); loomgtp Tx+Rx RawL3 pair + NetworkFor(dgram); appsession.go controller-lite (Capabilities gate, TimeSync→owd.Tracker, remote telemetry subscribe); StartApp/AppStream/StopApp RPCs + 'orbit ue app voip'; correlate.go; Prometheus app metrics.
- **Testbed demo:** A registered UE runs a 60s G.711 call through gNB→UPF→N6 loomd: Wireshark on N3 shows RTP inside GTP-U decoding cleanly; CLI streams per-second both-end MOS; an Xn handover mid-call yields 'HANDOVER @t → 240ms DL media gap [±err] → MOS 4.4→2.1 → recovered' in the report and as Grafana annotations; the N2 variant reproduces the SD-Core D-4 DL bug as a documented expected-fail signature.

### Phase 5 — multi-UE: SharedTunnel cutover (orbit)

- **Deliverable:** Session.tunnel() migrated to per-gNB SharedTunnel + Demux (the only 2152 bind per gNB; legacy Tunnel retired at cutover); atomic TEID Rebind on handover; EADDRINUSE fixed; gated on multisession/handover_data/pathswitch_data integration suites; two-UE concurrent-media regression test.
- **Testbed demo:** Two UEs attached through one gNB hold concurrent G.711 calls plus the ICMP latency probe with no port collision (previously EADDRINUSE); an Xn handover during one UE's call survives via TEID rebind while the other UE's call is unaffected; all existing datapath/handover integration tests pass on the shared path.

### Phase 6 — netstack TCP + real HTTP/TLS (loom v0.11 + orbit)

- **Deliverable:** loom core/netstack (per-gNB multi-address gVisor Stack, dpEndpoint over the frame contract, Network(ueIP) views, benchmark + timestamp-audit harness); core/app/httpx client + HTTPOrigin server (TLS/h2, objects + segment endpoints); orbit netstack bridge (lazy per-gNB Stack, AddAddress/RemoveAddress on attach/release) + 'orbit ue app http'; HttpMetrics end-to-end; published netstack-vs-kernel delta.
- **Testbed demo:** A UE fetches HTTPS objects from the N6 loomd HTTPOrigin through the tunnel — real TCP SYN and TLS 1.3 ClientHello visible inside GTP-U in Wireshark; TTFB/goodput/TLS-handshake metrics stream; a handover mid-download shows the TCP stall/recovery in the interval series; two UEs browse concurrently over one gNB socket and one shared gVisor stack.

### Phase 7 — video QoE + fleet scale (SIP queued next)

- **Deliverable:** core/app/vidstream ABR player over httpx; VideoMetrics + stall-event correlation; 'orbit ue app video'; fleet behaviors with app cohorts (dgram VoIP at scale, budgeted per-gNB netstacks for TCP cohorts), per-cohort aggregate MOS/QoE (p5/p50/p95) in FleetReport + Prometheus; ADR + design note for the 'sip' app driving voip.MediaConfig (implementation next).
- **Testbed demo:** 'orbit ue app video' plays a 3-minute synthetic HLS-style ladder from the N6 HTTPOrigin; buffer-level timeline in the report; an Xn handover produces 'handover → buffer drain → 1.8s stall → ABR downshift' correlated annotations; fleet run: 50 UEs in mixed voip/web/video cohorts with per-cohort MOS/stall dashboards and no RTCP sync storms.

---

## 13. Risk register

- **SD-Core N2 handover downlink bug (orbit D-4: SMF programs target FAR TEID 0) makes the flagship 'handover vs live media' demo show a permanent DL blackhole rather than a transient MOS dip — misreadable as an orbit/loom defect.** → Xn path-switch (proven data continuity on this testbed) is the primary continuity demo; the N2 run is kept as an explicitly-labeled core-bug reproduction where the correlation report pinpoints 'DL gap begins at handover, never recovers' — a diagnostic selling point; upstream SD-Core issue tracked and a future N2 continuity demo gated on its fix.
- **MOS correctness: subtle E-model or RFC 3550 statistics errors produce plausible-but-wrong numbers — the worst failure mode for a measurement tool.** → Pure-function packages with golden tests pinned to G.107 Table 4 verification examples and R=93.2±0.01 defaults; RFC 3550 A.1/A.3/A.8 spec-vector tests and the naive-impl checklist embedded in package docs; live cross-check against Wireshark RTP stream analysis (jitter/loss must match) as a phase acceptance test; E-model Components breakdown exposed in every result for auditability; Opus scored via G.107.1 with provisional Ie,wb flagged until calibrated, G.711 is the quoted headline number.
- **One-way-delay accuracy limited by software clock sync; asymmetric paths (GTP-U one way, mgmt LAN the other) could bias offsets and thus Id/MOS.** → TimeSync runs only over the symmetric management network, never through the tunnel; owd.Tracker min-delay filters and drift-fits with an ErrBound carried on every OWD/Ta-derived number end-to-end (proto, CLI, Prometheus); the E-model input clamps to the labeled RTT/2 tier when uncertainty exceeds threshold; hardware timestamps remain an additive upgrade via Frame.Meta.
- **The Demux/SharedTunnel migration touches the proven single-UE data path and handover TEID lifecycle; a demux bug could regress the most battle-tested part of orbit.** → Two-stage rollout: Phase 4 layers the Demux additively on the existing per-session Tunnel socket (no new bind, latency probe migrated with a like-for-like API), Phase 5 cuts over to per-gNB SharedTunnel as the sole 2152 bind, gated on the existing multisession/handover_data/pathswitch_data suites plus a new two-UE contention test; bounded rings with drop counters make interference observable instead of silent; no phase runs legacy and shared binds in parallel on one gNB.
- **gVisor dependency: large module, internal API churn, and per-stack memory at fleet scale; userspace TCP behavior could color measurements attributed to the RAN.** → One multi-address Stack per gNB (not per UE) with per-connection source binding — hundreds of UE addresses on one stack; gVisor pinned at a tested release and isolated in core/netstack (loom_nonetstack build tag for minimal agents); VoIP rides the lightweight dgram network so fleet voice never pays gVisor cost; Phase 6 gates on a published netstack-vs-kernel benchmark delta and a sender-side timestamp audit quantifying stack-induced jitter separately.
- **loom public-API churn (three new registries, two new roles, proto growth) burdens non-orbit users or draws maintainer pushback, especially the APP_* roles vs reusing RESPONDER.** → All wire changes additive per ADR-0021 (field numbers verified free against control.proto); each capability lands loom-first with an ADR + mkdocs page + contract tests and is demoable standalone ('loom rtp --call/--answer', memory-network CI); the roles-vs-selector decision is recorded in an ADR with the rejected alternative; orbit pins loom by tag per phase, never HEAD mid-phase; runtime skew guarded by the CapabilitiesResponse apps/networks fail-fast gate.
- **Symmetric-RTP rendezvous (answerer latches first source) could fail under startup loss or admit stray traffic on shared testbeds.** → Latch requires RTP validity + A.1 probation (2 in-order packets) on an (addr,SSRC) pair; other sources dropped and counted as stray_packets; caller enforces a bounded HandshakeTimeout surfaced as a typed session error; answerer binds inside port_min/port_max for deterministic firewalling; loom auth token gates Configure; far-end flows are duration-bounded orphan protection; the SIP phase replaces the latch with explicit SDP addresses.
- **Scope across two repos (RTP + E-model + netpath + netstack + HTTP + video + fleet) stalls delivery or produces half-integrated seams.** → Phases are sized to 1-3 PRs each and independently demoable (Phases 1-3 ship standalone loom value with zero orbit changes; Phase 0 is a one-PR orbit win); a single netpath.Network seam prevents parallel transport abstractions from accreting; the 'Loom issues to file' list scopes every loom work item as a reviewable unit; each orbit phase pins the loom tag it needs.
