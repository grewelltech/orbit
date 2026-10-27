package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/secrets"
)

// Known secrets for the archive test: none of them may reach a stored
// archive or a reply, wherever the request put them.
const (
	archKi = "5122250214c33e723a5dd523fc145fc0"
	// archKiUpper is archKi as a copy from another tool might have it.
	archKiUpper = "5122250214C33E723A5DD523FC145FC0"
	archOpc     = "981d464c7c52eb6e5036234984ad0bcf"
	archToken   = "loomtok-7f3a91c2e4b5d6a8"
)

// archFleetYAML carries the keys as values, a loom token, and the OPc and
// token again pasted into comments, where only the by-value scrub finds
// them.
const archFleetYAML = `
kind: fleet
name: archive-secrets
core:
  amf: 127.0.0.1:1
  plmn: { mcc: "208", mnc: "93" }
  tac: 1
  slice: { sst: 1, sd: "010203" }
  dnn: internet
# the lab sheet has it upper-cased: ` + archKiUpper + `
credentials:
  ki: "` + archKi + `"
  opc: "` + archOpc + `" # copied from the lab sheet: opc ` + archOpc + `
topology:
  gnbs:
    count: 1
    id_base: 1
    layout: grid
    source_ips: ["127.0.0.1"]
fleet:
  count: 1
  supi_base: "208930100007500"
  distribution: even
  attach_rate: 10/s
  pdu_session: true
behaviors:
  traffic:
    bulk: { ues: 1, uplink_rate: 1kbps, peer: "127.0.0.1:1", token: "` + archToken + `" } # token ` + archToken + `
`

// A fleet run and a load run, each carrying known keys (and the fleet a
// token, also in comments), leave no trace of them in the .orbitrun file's
// raw bytes, nor in GetRun/ListRuns, live or restored after a restart.
func TestArchivesHoldNoSecrets(t *testing.T) {
	dir := t.TempDir()
	serve := func() orbitv1connect.RunServiceClient {
		h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
	}
	c := serve()
	creds := func() *orbitv1.Credentials { return &orbitv1.Credentials{Ki: archKi, Opc: archOpc} }
	load := failFastLoadSpec()
	load.Credentials = creds()
	reqs := map[string]*orbitv1.StartRunRequest{
		"fleet": {Name: "fleet-secrets", Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ScenarioYaml: archFleetYAML, Credentials: creds()}}}, //nolint:staticcheck // the deprecated arm, kept for API clients
		"load":  {Name: "load-secrets", Spec: &orbitv1.StartRunRequest_Load{Load: load}},                                                                        //nolint:staticcheck // the deprecated arm, kept for API clients
	}
	ids := map[string]string{}
	// The runs share SUPIs and a gNB ID, and each holds them until its
	// refused dial gives up, so the load run starts once the fleet run's
	// archive is written.
	for _, kind := range []string{"fleet", "load"} {
		resp, err := c.StartRun(context.Background(), connect.NewRequest(reqs[kind]))
		if err != nil {
			t.Fatalf("StartRun(%s): %v", kind, err)
		}
		ids[kind] = resp.Msg.GetRun().GetRunId()
		if kind == "fleet" {
			waitArchive(t, dir, ids[kind])
		}
	}

	secretsIn := func(what string, b []byte) {
		t.Helper()
		for _, s := range []string{archKi, archKiUpper, archOpc, archToken} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds %q", what, s)
			}
		}
	}
	replies := func(c orbitv1connect.RunServiceClient, when string) {
		t.Helper()
		list, err := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := proto.Marshal(list.Msg)
		secretsIn("ListRuns "+when, b)
		for kind, id := range ids {
			got, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := proto.Marshal(got.Msg)
			secretsIn("GetRun("+kind+") "+when, b)
		}
	}
	replies(c, "while live")

	for kind, id := range ids {
		path := filepath.Join(dir, id+archiveExt)
		var raw []byte
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
				raw = b
				break
			}
		}
		if raw == nil {
			t.Fatalf("the %s run was never archived", kind)
		}
		secretsIn(kind+" .orbitrun", raw)

		// Masked, not dropped: the archive still says what was configured.
		a, err := readArchive(path)
		if err != nil {
			t.Fatal(err)
		}
		if found := secrets.Find(a); len(found) != 0 {
			t.Errorf("%s archive still has marked secrets: %v", kind, found)
		}
		var credsOut *orbitv1.Credentials
		if kind == "fleet" {
			credsOut = a.GetFleetSpec().GetCredentials()
			doc := a.GetFleetSpec().GetScenarioYaml()
			if !strings.Contains(doc, "name: archive-secrets") || !strings.Contains(doc, "copied from the lab sheet") {
				t.Errorf("the archived scenario lost more than its secrets:\n%s", doc)
			}
		} else {
			credsOut = a.GetLoadSpec().GetCredentials()
			if a.GetLoadSpec().GetBaseImsi() != load.GetBaseImsi() {
				t.Error("the archived load spec lost more than its secrets")
			}
		}
		if credsOut.GetKi() != secrets.Masked || credsOut.GetOpc() != secrets.Masked {
			t.Errorf("%s archive credentials = %v, want masked", kind, credsOut)
		}
	}
	replies(c, "after archiving")
	replies(serve(), "after a restart")
}

// An archive written before marked fields were masked (an older build) is
// masked as it is loaded, so it is never served with a key in a marked field.
func TestOldArchiveIsMaskedOnLoad(t *testing.T) {
	dir := t.TempDir()
	a := &orbitv1.RunArchive{
		Version: archiveVersion,
		Run:     &orbitv1.Run{RunId: "run-0ld", State: orbitv1.RunState_RUN_STATE_FAILED},
		Spec: &orbitv1.RunArchive_FleetSpec{FleetSpec: &orbitv1.FleetRunSpec{ //nolint:staticcheck // the deprecated arm, kept for API clients
			ScenarioYaml: "credentials: {ki: " + archKi + "}\n",
			Credentials:  &orbitv1.Credentials{Ki: archKi, Opc: archOpc},
		}},
	}
	b, err := proto.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run-0ld"+archiveExt)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = proto.Marshal(got)
	for _, s := range []string{archKi, archOpc} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("a loaded old archive still holds %q", s)
		}
	}
}
