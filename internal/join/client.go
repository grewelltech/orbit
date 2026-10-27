package join

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// Config is a registered responder's file (/etc/orbit/responder.yaml): how
// to reach its server, who it is, and its two secrets. Mode 0600; with
// systemd it reaches the service through LoadCredential.
type Config struct {
	Server     string `yaml:"server"`
	Pin        string `yaml:"pin"`
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Control    string `yaml:"control"`
	Credential string `yaml:"credential"`
	LoomToken  string `yaml:"loom_token"`
}

// pending is a registration in flight (<config>.pending): its fresh
// credential, and the existing one it rotates, written before the request
// so a retry presents the same credential.
type pending struct {
	Credential         string `yaml:"credential"`
	ExistingCredential string `yaml:"existing_credential,omitempty"`
}

// ReadSecretFile reads a file holding a secret, refusing one other users
// can read or write.
func ReadSecretFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("%s is open to other users (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	return os.ReadFile(path)
}

// LoadConfig reads a responder config.
func LoadConfig(path string) (Config, error) {
	b, err := ReadSecretFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case c.Server == "" || c.Pin == "" || c.ID == "" || c.Control == "":
		return Config{}, fmt.Errorf("%s: needs server, pin, id and control", path)
	case !ValidCredential(c.Credential):
		return Config{}, fmt.Errorf("%s: credential is not a responder credential", path)
	case !strings.HasPrefix(c.LoomToken, LoomTokenPrefix):
		return Config{}, fmt.Errorf("%s: loom_token is not a loom token", path)
	}
	return c, nil
}

// writeSecret writes v as YAML to path atomically, mode 0600, owned by uid
// and gid (-1 leaves them).
func writeSecret(path string, v any, uid, gid int) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Client is a JoinService client for an app host: TLS pinned to the
// server's key (sha256/<base64>), whatever its certificate says.
func Client(server, pin string) (orbitv1connect.JoinServiceClient, error) {
	if !pinRE.MatchString(pin) {
		return nil, errors.New("pin: want sha256/<base64>")
	}
	if !strings.HasPrefix(server, "https://") {
		return nil, fmt.Errorf("server %q: want https://host:port", server)
	}
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: tlspin.VerifyPin(pin)}, // #nosec G402 -- the pin is the check
	}}
	return orbitv1connect.NewJoinServiceClient(hc, server), nil
}

// Registration is what `orbit responder register` sends, besides the
// secrets.
type Registration struct {
	Server, Pin       string
	Name              string
	Control, DataIP   string
	PortLow, PortHigh uint32
	Version           string
	// UID and GID own the config (-1: leave them), for a unit that reads it
	// as its own user rather than through LoadCredential.
	UID, GID int
}

// CredentialState says whether a registration uses a new credential or
// re-registers an existing responder; the join script's plan shows it.
func CredentialState(configPath string) string {
	if _, err := os.Stat(configPath); err == nil {
		return "existing (re-registration)"
	}
	return "new"
}

// Register registers this host's responder with joinToken (ADR-0009 §3)
// and writes configPath. The credential is generated here and written to
// <configPath>.pending before the request, so a retry (here, or a rerun
// of the join script) presents the same one and gets the same answer. An
// existing configPath makes it a re-registration that rotates both
// secrets. On success the config replaces configPath atomically and the
// pending file goes.
func Register(ctx context.Context, client orbitv1connect.JoinServiceClient, joinToken, configPath string, r Registration) (Config, error) {
	pendingPath := configPath + ".pending"
	var p pending
	if b, err := ReadSecretFile(pendingPath); err == nil {
		if err := yaml.Unmarshal(b, &p); err != nil || !ValidCredential(p.Credential) {
			return Config{}, fmt.Errorf("%s is not a pending registration; remove it to start again", pendingPath)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if old, err := LoadConfig(configPath); err == nil {
			p.ExistingCredential = old.Credential
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		c, err := NewCredential()
		if err != nil {
			return Config{}, err
		}
		p.Credential = c
		if err := writeSecret(pendingPath, p, -1, -1); err != nil {
			return Config{}, err
		}
	} else {
		return Config{}, err
	}
	req := &orbitv1.RegisterResponderRequest{JoinToken: joinToken, Credential: p.Credential, ExistingCredential: p.ExistingCredential,
		Name: r.Name, ControlAddress: r.Control, DataIp: r.DataIP, DataPortLow: r.PortLow, DataPortHigh: r.PortHigh, Version: r.Version}
	var res *connect.Response[orbitv1.RegisterResponderResponse]
	var err error
	for try := 0; try < 3; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return Config{}, ctx.Err()
			case <-time.After(time.Duration(try) * time.Second):
			}
		}
		res, err = client.RegisterResponder(ctx, connect.NewRequest(req))
		// A Connect error is the server's answer: retrying it changes
		// nothing. Only a transport failure is tried again.
		if err == nil || connect.CodeOf(err) != connect.CodeUnavailable && connect.CodeOf(err) != connect.CodeUnknown {
			break
		}
	}
	if err != nil {
		return Config{}, err
	}
	c := Config{Server: r.Server, Pin: r.Pin, ID: res.Msg.GetResponderId(), Name: res.Msg.GetName(), Control: r.Control,
		Credential: p.Credential, LoomToken: res.Msg.GetLoomToken()}
	if err := writeSecret(configPath, c, r.UID, r.GID); err != nil {
		return Config{}, err
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	return c, nil
}

// heartbeat sends one heartbeat with credential.
func heartbeat(ctx context.Context, client orbitv1connect.JoinServiceClient, credential string, msg *orbitv1.HeartbeatRequest) (*orbitv1.HeartbeatResponse, error) {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Responder "+credential)
	res, err := client.Heartbeat(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// removed reports whether err is the server saying this responder was
// removed: the one answer that stops it.
func removed(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce) && ce.Code() == connect.CodeUnauthenticated && ce.Meta().Get("Orbit-Responder-State") == "removed"
}

// Heartbeater keeps a responder's heartbeat going (ADR-0009 §3).
type Heartbeater struct {
	Client     orbitv1connect.JoinServiceClient
	Credential string
	ID         string
	Version    string
	// Listening reports whether the loom control listener is up.
	Listening func() bool
	Log       *slog.Logger
	// after and jitter are the clock and the randomness; tests replace them.
	after  func(time.Duration) <-chan time.Time
	jitter func() float64
}

// Backoff after a failed heartbeat: from backoffMin, doubling, to
// backoffMax.
const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// jittered is d ±20%.
func (h *Heartbeater) jittered(d time.Duration) time.Duration {
	f := rand.Float64
	if h.jitter != nil {
		f = h.jitter
	}
	return time.Duration(float64(d) * (0.8 + 0.4*f()))
}

// Run beats until ctx ends (nil) or the server says this responder was
// removed (ErrResponderRemoved). Any other failure, an unknown credential
// included (a server restarted without its state, say), is retried with
// backoff and logged at warn: only an explicit removal is final.
func (h *Heartbeater) Run(ctx context.Context) error {
	after := time.After
	if h.after != nil {
		after = h.after
	}
	start := time.Now()
	interval, backoff := HeartbeatInterval, time.Duration(0)
	lastErr := ""
	for {
		listening := h.Listening == nil || h.Listening()
		res, err := heartbeat(ctx, h.Client, h.Credential, &orbitv1.HeartbeatRequest{Version: h.Version,
			LoomListening: listening, UptimeSeconds: uint64(time.Since(start).Seconds())})
		var wait time.Duration
		switch {
		case err == nil:
			if lastErr != "" {
				h.Log.Info("heartbeat restored", "id", h.ID)
				lastErr = ""
			}
			if s := res.GetHeartbeatSeconds(); s > 0 {
				interval = time.Duration(s) * time.Second
			}
			backoff = 0
			wait = interval
		case removed(err):
			h.Log.Error("this responder was removed from its server (orbit responder remove); stopping", "id", h.ID)
			return ErrResponderRemoved
		case ctx.Err() != nil:
			return nil
		default:
			if backoff == 0 {
				backoff = backoffMin
			} else if backoff *= 2; backoff > backoffMax {
				backoff = backoffMax
			}
			msg := err.Error()
			if connect.CodeOf(err) == connect.CodeUnauthenticated {
				msg = "the server does not know this responder (id " + h.ID + "); re-join with orbit join create, or restore the server's state dir"
			}
			if msg != lastErr {
				h.Log.Warn("heartbeat failed; retrying", "id", h.ID, "err", msg, "retry_in", backoff)
				lastErr = msg
			}
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-after(h.jittered(wait)):
		}
	}
}

// Check waits up to wait for the server to have seen a heartbeat from this
// responder's own service since Check started: probes ask without counting
// as one. It returns the server's word for why not, on failure.
func Check(ctx context.Context, client orbitv1connect.JoinServiceClient, credential string, wait time.Duration) error {
	start := time.Now()
	deadline := start.Add(wait)
	var last error
	for {
		res, err := heartbeat(ctx, client, credential, &orbitv1.HeartbeatRequest{Probe: true})
		switch {
		case removed(err):
			return ErrResponderRemoved
		case err != nil:
			last = err
			if connect.CodeOf(err) == connect.CodeUnauthenticated {
				last = errors.New("the server does not know this responder; re-join with orbit join create, or restore the server's state dir")
			}
		case res.GetLastSeenUnixNano() >= start.Add(-2*time.Second).UnixNano():
			return nil
		default:
			last = errors.New("the service has not sent a heartbeat yet")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no heartbeat from the responder service within %s: %w", wait, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ParsePorts parses LO-HI.
func ParsePorts(s string) (lo, hi uint32, err error) {
	a, b, ok := strings.Cut(s, "-")
	l, err1 := strconv.ParseUint(a, 10, 16)
	h, err2 := strconv.ParseUint(b, 10, 16)
	if !ok || err1 != nil || err2 != nil || l == 0 || l > h {
		return 0, 0, fmt.Errorf("data ports %q: want LO-HI", s)
	}
	return uint32(l), uint32(h), nil
}
