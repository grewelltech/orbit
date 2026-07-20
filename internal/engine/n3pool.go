package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/loomgtp"
)

// n3Pool owns the per-gNB shared N3 tunnels (design §6, Phase 5): ONE UDP
// socket bound per gNB N3 address, created when the first session needs a
// data path on that gNB and closed when the last releases it. Sessions never
// bind their own N3 socket — that is what removed the EADDRINUSE collision
// between UEs on one gNB.
type n3Pool struct {
	mu   sync.Mutex
	tuns map[string]*sharedN3

	// grace delays releaseAfterGrace's ref drop (the inter-gNB handover
	// path): the source socket must outlive the move by the End-Marker
	// window or the UPF's post-path-switch End Marker on the vacated TEID
	// (TS 29.281 §7.3) hits a closed port when the mover was the source
	// gNB's last UE. Tests may zero it for immediate release.
	grace time.Duration
}

// sharedN3 is one refcounted pool entry: the gNB's SharedTunnel keyed by its
// local N3 bind address, plus the gNB's lazy netstack bridge (design §6 —
// the ONE gVisor stack serving every TCP app on this gNB). The bridge shares
// the entry's refcount lifecycle: created on the first TCP-app need, closed
// with the shared tunnel when the last session releases the entry.
type sharedN3 struct {
	key  string // local N3 bind address ("host:port") — the pool key
	st   *datapath.SharedTunnel
	refs int

	nsMu sync.Mutex
	ns   *loomgtp.GNBStack
}

// netstack lazily creates (and caches) the gNB's netstack bridge — the
// first TCP app on any of the gNB's sessions pays the gVisor cost, UDP-only
// gNBs never do.
func (r *sharedN3) netstack() (*loomgtp.GNBStack, error) {
	r.nsMu.Lock()
	defer r.nsMu.Unlock()
	if r.ns == nil {
		g, err := loomgtp.NewGNBStack(loomgtp.DefaultInnerMTU)
		if err != nil {
			return nil, err
		}
		r.ns = g
	}
	return r.ns, nil
}

// closeNetstack closes the entry's netstack bridge, if one was created.
func (r *sharedN3) closeNetstack() {
	r.nsMu.Lock()
	ns := r.ns
	r.ns = nil
	r.nsMu.Unlock()
	if ns != nil {
		_ = ns.Close()
	}
}

func newN3Pool() *n3Pool {
	return &n3Pool{tuns: make(map[string]*sharedN3), grace: datapath.EndMarkerGraceTTL}
}

// acquire returns the shared tunnel bound to localN3, creating (and binding)
// it on first use. Every acquire must be paired with exactly one release.
func (p *n3Pool) acquire(localN3, upfN3 string) (*sharedN3, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.tuns[localN3]; ok {
		r.refs++
		return r, nil
	}
	st, err := datapath.NewSharedTunnel(localN3, upfN3)
	if err != nil {
		return nil, fmt.Errorf("open shared N3 data path %s: %w", localN3, err)
	}
	r := &sharedN3{key: localN3, st: st, refs: 1}
	p.tuns[localN3] = r
	return r, nil
}

// release drops one reference; the last release closes the socket (and its
// Demux — remaining lanes would be closed, but by then no session holds one).
// nil is a no-op so teardown paths need no guards.
func (p *n3Pool) release(r *sharedN3) {
	if r == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r.refs--
	if r.refs > 0 {
		return
	}
	// A deferred (grace) release may land after a new tunnel was created
	// under the same key: only remove the entry we actually own.
	if cur, ok := p.tuns[r.key]; ok && cur == r {
		delete(p.tuns, r.key)
	}
	// The gNB's netstack bridge dies with its shared tunnel (design Phase 6
	// lifecycle): close it first so its receive loop and gVisor workers wind
	// down before the socket that fed them goes away.
	r.closeNetstack()
	_ = r.st.Close()
}

// releaseAfterGrace releases r after the pool's End-Marker grace window (the
// inter-gNB handover source-side drop). With no grace configured it releases
// immediately. nil is a no-op.
func (p *n3Pool) releaseAfterGrace(r *sharedN3) {
	if r == nil {
		return
	}
	if p.grace <= 0 {
		p.release(r)
		return
	}
	time.AfterFunc(p.grace, func() { p.release(r) })
}

// size reports the live shared tunnels (tests).
func (p *n3Pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tuns)
}

// netstackCount reports how many pool entries hold a live netstack bridge —
// the per-gNB gVisor budget assertion (design §8): TCP app cohorts share ONE
// Stack per gNB N3 socket, so this may never exceed size(), no matter how
// many UEs run TCP apps.
func (p *n3Pool) netstackCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.tuns {
		r.nsMu.Lock()
		if r.ns != nil {
			n++
		}
		r.nsMu.Unlock()
	}
	return n
}
