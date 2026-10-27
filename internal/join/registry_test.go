package join

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// regLab is a token store and a registry on one settable clock.
type regLab struct {
	clk   *clock
	store *Store
	reg   *Registry
}

func newRegLab(t *testing.T, dir string) regLab {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	reg, err := OpenRegistry(dir, WithRegistryClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	return regLab{clk: clk, store: NewStore(WithClock(clk.now)), reg: reg}
}

// token issues a fresh join token.
func (l regLab) token(t *testing.T) string {
	t.Helper()
	_, secret, err := l.store.Create("", 0, testSpec)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func cred(t *testing.T) string {
	t.Helper()
	c, err := NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func regReq(tok, credential, name string) RegisterRequest {
	return RegisterRequest{JoinToken: tok, Credential: credential, Name: name, Control: "198.51.100.20:9551",
		DataIP: "198.51.100.20", PortLow: 32768, PortHigh: 60999, Version: "test", Source: "198.51.100.20:40000"}
}

// Of many registrations racing with one join token, exactly one burns it;
// every other gets the uniform unknown-token answer.
func TestRegisterBurnRace(t *testing.T) {
	l := newRegLab(t, "")
	tok := l.token(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.reg.Register(l.store, regReq(tok, cred(t), "app"+string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrUnknownToken):
			t.Errorf("a loser got %v, want ErrUnknownToken", err)
		}
	}
	if won != 1 || len(l.reg.List()) != 1 {
		t.Errorf("%d registrations won, %d responders; want 1 and 1", won, len(l.reg.List()))
	}
}

// The registration rules, each against a fresh lab.
func TestRegister(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, l regLab) error
		want string // "" = success; else a substring of the error
	}{
		{name: "a new responder", run: func(t *testing.T, l regLab) error {
			rs, err := l.reg.Register(l.store, regReq(l.token(t), cred(t), "app1"))
			if err == nil && (!strings.HasPrefix(rs.ID, "rs-") || !strings.HasPrefix(rs.LoomToken, LoomTokenPrefix) || rs.JoinTokenID == "") {
				t.Errorf("responder %+v", rs)
			}
			return err
		}},
		{name: "a retry with the same credential gets the same answer", run: func(t *testing.T, l regLab) error {
			tok, c := l.token(t), cred(t)
			a, err := l.reg.Register(l.store, regReq(tok, c, "app1"))
			if err != nil {
				return err
			}
			b, err := l.reg.Register(l.store, regReq(tok, c, "app1"))
			if err == nil && (a.ID != b.ID || a.LoomToken != b.LoomToken) {
				t.Errorf("retry: %s/%s, first %s/%s", b.ID, b.LoomToken, a.ID, a.LoomToken)
			}
			return err
		}},
		{name: "a used token with another credential", want: "unknown or expired", run: func(t *testing.T, l regLab) error {
			tok := l.token(t)
			if _, err := l.reg.Register(l.store, regReq(tok, cred(t), "app1")); err != nil {
				return err
			}
			_, err := l.reg.Register(l.store, regReq(tok, cred(t), "app2"))
			return err
		}},
		{name: "an expired token", want: "unknown or expired", run: func(t *testing.T, l regLab) error {
			tok := l.token(t)
			l.clk.t = l.clk.t.Add(DefaultTTL + time.Second)
			_, err := l.reg.Register(l.store, regReq(tok, cred(t), "app1"))
			return err
		}},
		{name: "a revoked token", want: "unknown or expired", run: func(t *testing.T, l regLab) error {
			tok := l.token(t)
			tk, _ := l.store.Lookup(tok)
			if err := l.store.Revoke(tk.ID); err != nil {
				return err
			}
			_, err := l.reg.Register(l.store, regReq(tok, cred(t), "app1"))
			return err
		}},
		{name: "a taken name, refused before the burn", want: "responder name app1 is taken", run: func(t *testing.T, l regLab) error {
			if _, err := l.reg.Register(l.store, regReq(l.token(t), cred(t), "app1")); err != nil {
				return err
			}
			tok := l.token(t)
			_, err := l.reg.Register(l.store, regReq(tok, cred(t), "app1"))
			// The token was not used up: another name takes it.
			if _, err2 := l.reg.Register(l.store, regReq(tok, cred(t), "app2")); err2 != nil {
				t.Errorf("the refused name burned the token: %v", err2)
			}
			return err
		}},
		{name: "re-registration rotates both secrets and keeps the ID", run: func(t *testing.T, l regLab) error {
			old := cred(t)
			a, err := l.reg.Register(l.store, regReq(l.token(t), old, "app1"))
			if err != nil {
				return err
			}
			q := regReq(l.token(t), cred(t), "app1")
			q.ExistingCredential = old
			b, err := l.reg.Register(l.store, q)
			if err != nil {
				return err
			}
			if a.ID != b.ID || a.LoomToken == b.LoomToken || a.CredentialHash == b.CredentialHash || len(l.reg.List()) != 1 {
				t.Errorf("re-registration: %+v after %+v", b, a)
			}
			if _, _, err := l.reg.Heartbeat(old, "x", "", true, false); !errors.Is(err, ErrUnknownResponder) {
				t.Errorf("the old credential still beats: %v", err)
			}
			return nil
		}},
		{name: "re-registration needs a valid token: a credential alone does nothing", want: "unknown or expired", run: func(t *testing.T, l regLab) error {
			old := cred(t)
			if _, err := l.reg.Register(l.store, regReq(l.token(t), old, "app1")); err != nil {
				return err
			}
			q := regReq(Prefix+"AAAAAAAAAAAAAAAAAAAAAA", cred(t), "app1")
			q.ExistingCredential = old
			_, err := l.reg.Register(l.store, q)
			return err
		}},
		{name: "an existing credential the server does not hold, refused before the burn", want: "not one this server knows", run: func(t *testing.T, l regLab) error {
			tok := l.token(t)
			q := regReq(tok, cred(t), "app1")
			q.ExistingCredential = cred(t)
			_, err := l.reg.Register(l.store, q)
			if _, err2 := l.reg.Register(l.store, regReq(tok, cred(t), "app1")); err2 != nil {
				t.Errorf("the refusal burned the token: %v", err2)
			}
			return err
		}},
		{name: "not a credential", want: "credential", run: func(t *testing.T, l regLab) error {
			_, err := l.reg.Register(l.store, regReq(l.token(t), "orc1_short", "app1"))
			return err
		}},
		{name: "a bad name", want: "name", run: func(t *testing.T, l regLab) error {
			_, err := l.reg.Register(l.store, regReq(l.token(t), cred(t), "app 1;id"))
			return err
		}},
		{name: "a control address with a zone", want: "control_address", run: func(t *testing.T, l regLab) error {
			q := regReq(l.token(t), cred(t), "app1")
			q.Control = "[fe80::1%x]:9551"
			_, err := l.reg.Register(l.store, q)
			return err
		}},
		{name: "an inverted port range", want: "port range", run: func(t *testing.T, l regLab) error {
			q := regReq(l.token(t), cred(t), "app1")
			q.PortLow, q.PortHigh = 60999, 32768
			_, err := l.reg.Register(l.store, q)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t, newRegLab(t, ""))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// Heartbeats: online after one, offline after three missed intervals; a
// probe counts for nothing; a removed responder's credential is told so
// (a tombstone), an unknown one is only unknown.
func TestHeartbeatAndResolve(t *testing.T) {
	l := newRegLab(t, "")
	c := cred(t)
	rs, err := l.reg.Register(l.store, regReq(l.token(t), c, "app1"))
	if err != nil {
		t.Fatal(err)
	}
	var off *OfflineError
	if _, err := l.reg.Resolve("app1"); !errors.As(err, &off) || !strings.Contains(err.Error(), "no heartbeat since the server started") {
		t.Errorf("never seen: %v", err)
	}
	if _, prev, err := l.reg.Heartbeat(c, "198.51.100.20:1", "v1", true, true); err != nil || !prev.IsZero() {
		t.Errorf("probe: %v, %v", prev, err)
	}
	if _, err := l.reg.Resolve("app1"); !errors.As(err, &off) {
		t.Errorf("a probe made it online: %v", err)
	}
	id, _, err := l.reg.Heartbeat(c, "198.51.100.20:1", "v1", true, false)
	if err != nil || id != rs.ID {
		t.Fatalf("heartbeat: %s, %v", id, err)
	}
	if got, err := l.reg.Resolve("app1"); err != nil || got.LoomToken != rs.LoomToken {
		t.Errorf("online: %v", err)
	}
	if st := l.reg.List(); st[0].State != StateOnline || st[0].LastSource != "198.51.100.20:1" {
		t.Errorf("list: %+v", st[0])
	}
	l.clk.t = l.clk.t.Add(OfflineAfter*HeartbeatInterval + time.Second)
	if _, err := l.reg.Resolve("app1"); !errors.As(err, &off) || !strings.Contains(err.Error(), "last heartbeat 31s ago from 198.51.100.20:1") {
		t.Errorf("offline: %v", err)
	}
	if _, err := l.reg.Resolve("nope"); !errors.Is(err, ErrNoResponder) {
		t.Errorf("unknown name: %v", err)
	}
	if _, _, err := l.reg.Heartbeat(cred(t), "x", "", true, false); !errors.Is(err, ErrUnknownResponder) {
		t.Errorf("unknown credential: %v", err)
	}
	if _, err := l.reg.Remove("app1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.reg.Heartbeat(c, "x", "", true, false); !errors.Is(err, ErrResponderRemoved) {
		t.Errorf("removed credential: %v", err)
	}
	if _, err := l.reg.Remove("app1"); !errors.Is(err, ErrNoResponder) {
		t.Errorf("remove twice: %v", err)
	}
}

// The registry survives a restart in its state directory (0600), with its
// tombstones; heartbeats do not, so every responder starts never seen. A
// file that cannot be read is an error, not an empty registry.
func TestRegistryPersists(t *testing.T) {
	dir := t.TempDir()
	l := newRegLab(t, dir)
	keep, gone := cred(t), cred(t)
	a, err := l.reg.Register(l.store, regReq(l.token(t), keep, "app1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.reg.Register(l.store, regReq(l.token(t), gone, "app2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.reg.Heartbeat(keep, "x", "", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.reg.Remove("app2"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, RegistryFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("registry file: %v, %v", fi, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, RegistryFileName))
	if strings.Contains(string(b), keep) || strings.Contains(string(b), gone) {
		t.Error("the registry file holds a credential")
	}
	re, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := re.List()
	if len(st) != 1 || st[0].ID != a.ID || st[0].LoomToken != a.LoomToken || st[0].State != StateNeverSeen {
		t.Errorf("after a restart: %+v", st)
	}
	if _, _, err := re.Heartbeat(gone, "x", "", true, false); !errors.Is(err, ErrResponderRemoved) {
		t.Errorf("the tombstone did not survive: %v", err)
	}
	for _, bad := range []string{"{not json", `{"version": 2, "responders": []}`, `{"responders": []}`} {
		if err := os.WriteFile(filepath.Join(dir, RegistryFileName), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenRegistry(dir); err == nil || !strings.Contains(err.Error(), RegistryFileName) {
			t.Errorf("registry %q opened, or the error does not name the file: %v", bad, err)
		}
	}
}
