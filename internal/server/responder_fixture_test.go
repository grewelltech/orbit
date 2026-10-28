package server

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/join"
)

var updateResponders = flag.Bool("update-responders", false, "write ListResponders' and ListJoinTokens' answers for the dashboard's tests")

// respondersFixtureDir is where the Targets page's responder tests read
// real ListResponders and ListJoinTokens answers.
var respondersFixtureDir = filepath.Join("..", "..", "web", "src", "data", "join", "testdata")

// ListResponders and ListJoinTokens as the Targets page reads them: a
// responder in each state (online, offline, never seen) and a pending
// token, through the handlers. Neither answer carries a credential or a
// loom token; -update-responders writes them for the dashboard's tests.
func TestRespondersFixture(t *testing.T) {
	clk := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	reg, err := join.OpenRegistry("", join.WithRegistryClock(now))
	if err != nil {
		t.Fatal(err)
	}
	store := join.NewStore(join.WithClock(now))
	spec := join.Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"}
	creds := map[string]string{}
	for i, name := range []string{"app1", "app2", "app3"} {
		_, secret, err := store.Create(name, 0, spec)
		if err != nil {
			t.Fatal(err)
		}
		cred := newCred(t)
		ip := "198.51.100." + string(rune('2'+i)) + "0"
		if _, err := reg.Register(store, join.RegisterRequest{JoinToken: secret, Credential: cred, Name: name, Control: ip + ":9551",
			DataIP: ip, PortLow: 32768, PortHigh: 60999, Version: "v0.9.0", Source: ip + ":40000"}); err != nil {
			t.Fatal(err)
		}
		creds[name] = cred
	}
	// app2 beat once, two minutes ago: offline. app1 beats now: online.
	// app3 never has.
	if _, _, err := reg.Heartbeat(creds["app2"], "198.51.100.30:40001", "v0.9.0", true, false); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(2 * time.Minute)
	if _, _, err := reg.Heartbeat(creds["app1"], "198.51.100.20:40001", "v0.9.1", true, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("app4", 0, spec); err != nil { // pending
		t.Fatal(err)
	}

	rs, err := (&responderService{reg: reg}).ListResponders(context.Background(), connect.NewRequest(&orbitv1.ListRespondersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := (&joinService{store: store}).ListJoinTokens(context.Background(), connect.NewRequest(&orbitv1.ListJoinTokensRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, r := range rs.Msg.GetResponders() {
		states[r.GetName()] = r.GetState()
	}
	if states["app1"] != join.StateOnline || states["app2"] != join.StateOffline || states["app3"] != join.StateNeverSeen {
		t.Fatalf("states %v", states)
	}
	if len(ts.Msg.GetTokens()) != 1 || ts.Msg.GetTokens()[0].GetName() != "app4" {
		t.Fatalf("pending tokens %v: the used ones are not pending", ts.Msg.GetTokens())
	}
	for name, m := range map[string]proto.Message{"responders": rs.Msg, "tokens": ts.Msg} {
		b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		for _, cred := range creds {
			if strings.Contains(string(b), cred) {
				t.Fatalf("%s carries a credential", name)
			}
		}
		if *updateResponders {
			if err := os.MkdirAll(respondersFixtureDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(respondersFixtureDir, name+".json"), append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
