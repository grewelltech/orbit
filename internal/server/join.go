package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/join"
)

// Join endpoint limits (ADR-0009 §1): per peer, at most joinPeerLimit
// requests a minute, script fetches and binaries alike; server-wide, at
// most joinMaxRenders scripts rendering and joinMaxBinaries binaries
// downloading at once. A token miss also counts toward the peer's lockout
// (security.failWith), as a wrong bearer token does.
const (
	joinPeerLimit   = 30
	joinPeerWindow  = time.Minute
	joinMaxRenders  = 16
	joinMaxBinaries = 4
)

// Join refusals. The not-found answer is the same for a token that is
// unknown, expired or revoked, and for any path but the two a token has.
const (
	msgJoinNeedsTLS = "join needs orbit serve with TLS (--tls auto, or --tls-cert/--tls-key)"
	msgJoinNotFound = "unknown or expired join token"
	msgJoinBusy     = "too many join requests; try again shortly"
	msgJoinNoMgmt   = "give --mgmt: the address the app host reaches this server at (the server listens on %s)"
)

// joinBinary is an orbit binary the join endpoint serves for one arch.
type joinBinary struct {
	path   string
	sha256 string
	size   int64
}

// joinService issues join tokens (JoinService) and serves what a token
// unlocks: the join script and the orbit binary it downloads.
type joinService struct {
	log    *slog.Logger
	sec    *security
	store  *join.Store
	reg    *join.Registry
	tls    bool   // orbit serve has TLS: without it nothing join is served
	pin    string // the server's key, sha256/<base64>
	listen string // the server's listen address, host:port
	bins   map[string]joinBinary

	renders, binaries chan struct{}

	mu    sync.Mutex
	peers map[string][]time.Time // requests by peerKey, within joinPeerWindow
}

func newJoinService(log *slog.Logger, sec *security, opts Options, reg *join.Registry) *joinService {
	j := &joinService{log: log, sec: sec, store: join.NewStore(), reg: reg, tls: opts.TLS, pin: opts.TLSPin, listen: opts.Listen,
		bins: map[string]joinBinary{}, renders: make(chan struct{}, joinMaxRenders),
		binaries: make(chan struct{}, joinMaxBinaries), peers: map[string][]time.Time{}}
	for arch, path := range opts.JoinBinaries {
		b, err := hashBinary(path)
		if err != nil {
			log.Error("join: no orbit binary for this arch", "arch", arch, "path", path, "err", err)
			continue
		}
		j.bins[arch] = b
	}
	return j
}

// hashBinary is the binary at path with its SHA-256, read once at startup:
// a file changed afterwards fails the script's check rather than run.
func hashBinary(path string) (joinBinary, error) {
	f, err := os.Open(path)
	if err != nil {
		return joinBinary{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return joinBinary{}, err
	}
	return joinBinary{path: path, sha256: hex.EncodeToString(h.Sum(nil)), size: n}, nil
}

// --- JoinService ---

func (j *joinService) CreateJoinToken(ctx context.Context, req *connect.Request[orbitv1.CreateJoinTokenRequest]) (*connect.Response[orbitv1.CreateJoinTokenResponse], error) {
	// The server's TLS, and this request's own connection: the response
	// carries the token.
	if cs, _ := ctx.Value(connStateKey{}).(connState); !j.tls || !cs.tls {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msgJoinNeedsTLS))
	}
	m := req.Msg
	sp := m.GetSpec()
	mgmt, err := j.management(sp.GetManagementAddress())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	spec := join.Spec{UEPools: sp.GetUePools(), UPFN6: sp.GetUpfN6(), N6Subnet: sp.GetN6Subnet(), Management: mgmt}
	tok, secret, err := j.store.Create(m.GetName(), time.Duration(m.GetTtlSeconds())*time.Second, spec)
	switch {
	case errors.Is(err, join.ErrNameTaken):
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	url := "https://" + mgmt + "/join/" + secret
	curlPin := "sha256//" + strings.TrimPrefix(j.pin, "sha256/")
	j.log.Info("join token created", "id", tok.ID, "name", tok.Name, "expires", tok.Expires)
	return connect.NewResponse(&orbitv1.CreateJoinTokenResponse{
		Issued: joinTokenProto(tok), Secret: secret, Url: url, Pin: j.pin,
		OneLiner: fmt.Sprintf("curl -fsS --proto '=https' -k --pinnedpubkey '%s' '%s' | sudo sh", curlPin, url),
	}), nil
}

// management is the management address a token's script uses: the one
// given, with the server's port when it has none, else the server's own
// listen address when an app host could reach it (not a wildcard or
// loopback).
func (j *joinService) management(given string) (string, error) {
	_, port, err := net.SplitHostPort(j.listen)
	if err != nil {
		return "", fmt.Errorf("the server's listen address %q: %w", j.listen, err)
	}
	if given != "" {
		if _, _, err := net.SplitHostPort(given); err != nil {
			given = net.JoinHostPort(strings.Trim(given, "[]"), port)
		}
		return given, nil
	}
	host, _, _ := net.SplitHostPort(j.listen)
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && (a.IsUnspecified() || a.IsLoopback())) || host == "localhost" {
		return "", fmt.Errorf(msgJoinNoMgmt, j.listen)
	}
	return j.listen, nil
}

func (j *joinService) ListJoinTokens(context.Context, *connect.Request[orbitv1.ListJoinTokensRequest]) (*connect.Response[orbitv1.ListJoinTokensResponse], error) {
	out := &orbitv1.ListJoinTokensResponse{}
	for _, t := range j.store.List() {
		out.Tokens = append(out.Tokens, joinTokenProto(t))
	}
	return connect.NewResponse(out), nil
}

func (j *joinService) RevokeJoinToken(_ context.Context, req *connect.Request[orbitv1.RevokeJoinTokenRequest]) (*connect.Response[orbitv1.RevokeJoinTokenResponse], error) {
	if err := j.store.Revoke(req.Msg.GetId()); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	j.log.Info("join token revoked", "id", req.Msg.GetId())
	return connect.NewResponse(&orbitv1.RevokeJoinTokenResponse{}), nil
}

func joinTokenProto(t join.Token) *orbitv1.JoinToken {
	return &orbitv1.JoinToken{Id: t.ID, Name: t.Name, CreatedUnixNano: t.Created.UnixNano(), ExpiresUnixNano: t.Expires.UnixNano(),
		Spec: &orbitv1.JoinSpec{UePools: t.Spec.UEPools, UpfN6: t.Spec.UPFN6, N6Subnet: t.Spec.N6Subnet, ManagementAddress: t.Spec.Management}}
}

// --- /join/ ---

// ServeHTTP serves /join/<token> (the script) and /join/<token>/bin/<arch>
// (the orbit binary): over TLS only, GET or HEAD, the token only as that
// path segment (never a query or a header), with a uniform 404 for
// everything else. Nothing logged here holds the token: only its ID.
func (j *joinService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !j.tls || r.TLS == nil {
		http.Error(w, msgJoinNeedsTLS, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
		return
	}
	peer := peerKey(r.RemoteAddr)
	if j.sec.lockedOut(peer) || !j.admit(peer) {
		http.Error(w, msgJoinBusy, http.StatusTooManyRequests)
		return
	}
	// The path as sent must be the path as decoded: any percent-encoding
	// (an encoded '/', '.', or token character) is the uniform 404.
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/join/"), "/")
	var tok join.Token
	ok := r.URL.RawQuery == "" && r.URL.RawPath == "" && !strings.Contains(r.URL.EscapedPath(), "%") &&
		(len(segs) == 1 || (len(segs) == 3 && segs[1] == "bin"))
	if ok {
		tok, ok = j.store.Lookup(segs[0])
	}
	if !ok {
		j.sec.failWith(peer, "unknown join token")
		http.Error(w, msgJoinNotFound, http.StatusNotFound)
		return
	}
	if len(segs) == 1 {
		j.serveScript(w, r, tok, segs[0])
		return
	}
	j.serveBinary(w, r, tok, segs[2], peer)
}

// admit counts a request against its peer's per-minute limit.
func (j *joinService) admit(peer string) bool {
	now := j.sec.now()
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.peers) >= authMaxPeers {
		for k, ts := range j.peers {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= joinPeerWindow {
				delete(j.peers, k)
			}
		}
	}
	recent := j.peers[peer][:0]
	for _, t := range j.peers[peer] {
		if now.Sub(t) < joinPeerWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= joinPeerLimit || len(j.peers) >= authMaxPeers {
		j.peers[peer] = recent
		return false
	}
	j.peers[peer] = append(recent, now)
	return true
}

// acquire takes a slot of a server-wide cap without waiting.
func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (j *joinService) serveScript(w http.ResponseWriter, r *http.Request, tok join.Token, secret string) {
	if !acquire(j.renders) {
		http.Error(w, msgJoinBusy, http.StatusTooManyRequests)
		return
	}
	defer func() { <-j.renders }()
	sha := map[string]string{}
	for arch, b := range j.bins {
		sha[arch] = b.sha256
	}
	script, err := join.Script(join.ScriptData{ServerURL: "https://" + tok.Spec.Management, Pin: j.pin, Token: secret,
		Name: tok.Name, SHA256: sha, Spec: tok.Spec})
	if err != nil {
		j.log.Error("join: the script did not render", "id", tok.ID, "err", err)
		http.Error(w, "the join script did not render: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	j.log.Info("join script fetched", "id", tok.ID, "peer", peerKey(r.RemoteAddr))
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(script)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(script)
}

func (j *joinService) serveBinary(w http.ResponseWriter, r *http.Request, tok join.Token, arch, peer string) {
	b, ok := j.bins[arch]
	if !ok {
		// The uniform 404, and a miss toward the peer's lockout, as for an
		// unknown token: neither the answer nor the lockout may tell a valid
		// token from a guess. The server's log says why (the script itself
		// refuses an arch it has no SHA-256 for, before downloading).
		j.sec.failWith(peer, "join binary for an arch this server does not serve", "id", tok.ID, "arch", arch)
		http.Error(w, msgJoinNotFound, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(b.size, 10))
	w.Header().Set("X-Orbit-SHA256", b.sha256)
	if r.Method == http.MethodHead {
		return
	}
	if !acquire(j.binaries) {
		http.Error(w, msgJoinBusy, http.StatusTooManyRequests)
		return
	}
	defer func() { <-j.binaries }()
	f, err := os.Open(b.path)
	if err != nil {
		http.Error(w, "the orbit binary is not readable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = f.Close() }()
	j.log.Info("join binary fetched", "id", tok.ID, "arch", arch, "peer", peerKey(r.RemoteAddr))
	_, _ = io.Copy(w, io.LimitReader(f, b.size))
}
