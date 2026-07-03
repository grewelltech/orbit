// Package mockamf is an in-process AMF that speaks enough NGAP/NAS to bring
// a UE to 5GMM-REGISTERED without a real core. It exists to measure ORBIT's
// sim-capacity — the UE-actor cost at scale — independently of the core under
// test (DESIGN §1, the "sim capability" number; the D-6 spike). The UE talks
// to it over an in-process pipe (no sockets), so the measurement isolates the
// actor model rather than SCTP behaviour.
package mockamf

import (
	"errors"
	"sync"
)

// pipe is one end of an in-process NGAP transport pair, satisfying
// gnb.Transport. Writes go to the peer's read channel. Streams are not
// demultiplexed (a single UE per pipe), and the PPID is always NGAP.
type pipe struct {
	name   string
	out    chan []byte // messages this end sends (peer reads)
	in     chan []byte // messages this end reads
	closed chan struct{}
	once   *sync.Once // shared by both ends so either can close once
}

// newPipePair returns two connected endpoints: one for the UE (given to
// engine.Attach) and one for the AMF handler. They share a close signal so
// tearing down one end wakes the other.
func newPipePair() (ue, amf *pipe) {
	a := make(chan []byte, 8)
	b := make(chan []byte, 8)
	closed := make(chan struct{})
	once := &sync.Once{}
	ue = &pipe{name: "ue", out: a, in: b, closed: closed, once: once}
	amf = &pipe{name: "amf", out: b, in: a, closed: closed, once: once}
	return ue, amf
}

const ppidNGAP = 60

var errPipeClosed = errors.New("mockamf: pipe closed")

func (p *pipe) WriteNGAP(stream uint16, pdu []byte) error {
	b := append([]byte(nil), pdu...)
	select {
	case p.out <- b:
		return nil
	case <-p.closed:
		return errPipeClosed
	}
}

func (p *pipe) ReadMsg(buf []byte) (payload []byte, stream uint16, ppid uint32, err error) {
	select {
	case b := <-p.in:
		return b, 1, ppidNGAP, nil
	case <-p.closed:
		return nil, 0, 0, errPipeClosed
	}
}

func (p *pipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}
