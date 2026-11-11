package conformance

import (
	"context"
	"strings"
	"testing"
)

// needy is a check with declared needs, for the subscriber layout.
type needy struct {
	id    string
	needs CheckNeeds
}

func (n needy) ID() string                    { return n.id }
func (needy) SpecRefs() []SpecRef             { return nil }
func (needy) Run(context.Context, Env) Result { return Result{Verdict: Pass} }
func (n needy) Needs() CheckNeeds             { return n.needs }
func layoutRegistry(needs ...CheckNeeds) *Registry {
	r := &Registry{}
	for i, n := range needs {
		r.Register(needy{id: string(rune('A' + i)), needs: n})
	}
	return r
}

// Each check's block follows the one before it, a check with no UE takes
// none, and a check that registers several UEs in turn takes one each; the
// preflight's one subscriber follows the last block.
func TestSubscriberBlocks(t *testing.T) {
	r := layoutRegistry(CheckNeeds{UEs: 1}, CheckNeeds{}, CheckNeeds{UEs: 1, Subscribers: 3}, CheckNeeds{UEs: 2})
	want := []struct{ off, span int }{{0, 1}, {1, 0}, {1, 3}, {4, 2}}
	for i, w := range want {
		if off, span := r.subscriberBlock(i); off != w.off || span != w.span {
			t.Errorf("check %d: block (%d, %d), want (%d, %d)", i, off, span, w.off, w.span)
		}
	}
	if got := r.SubscriberSpan(); got != 7 {
		t.Errorf("SubscriberSpan = %d, want 7", got)
	}
	if off, span := r.preflightSubscriber(); off != 6 || span != 1 {
		t.Errorf("preflight subscriber (%d, %d), want (6, 1)", off, span)
	}
	if off, span := r.subscriberBlock(9); off != 0 || span != 0 {
		t.Errorf("unknown check: block (%d, %d), want none", off, span)
	}
}

func TestUESpecForCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      UESpec
		off, span int
		wantSUPI  string
		wantSubs  int
	}{
		{name: "no pool keeps the SUPI", spec: UESpec{SUPI: "001010100007500"}, off: 4, span: 1, wantSUPI: "001010100007500"},
		{name: "pool of one keeps the SUPI", spec: UESpec{SUPI: "001010100007500", Subscribers: 1}, off: 4, span: 1, wantSUPI: "001010100007500", wantSubs: 1},
		{name: "own block", spec: UESpec{SUPI: "001010100007500", Subscribers: 50}, off: 4, span: 3, wantSUPI: "001010100007504", wantSubs: 3},
		{name: "leading zeros and the NAI form", spec: UESpec{SUPI: "imsi-001010000000009", Subscribers: 50}, off: 1, span: 1, wantSUPI: "001010000000010", wantSubs: 1},
		{name: "small pool wraps", spec: UESpec{SUPI: "001010100007500", Subscribers: 4}, off: 9, span: 1, wantSUPI: "001010100007501", wantSubs: 1},
		{name: "block cut at the pool's end", spec: UESpec{SUPI: "001010100007500", Subscribers: 4}, off: 3, span: 3, wantSUPI: "001010100007503", wantSubs: 1},
		{name: "check without a UE", spec: UESpec{SUPI: "001010100007500", Subscribers: 50}, off: 7, span: 0, wantSUPI: "001010100007500", wantSubs: 50},
		{name: "bad SUPI left for the harness", spec: UESpec{SUPI: "nope", Subscribers: 50}, off: 2, span: 1, wantSUPI: "nope", wantSubs: 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.forCheck(tc.off, tc.span)
			if got.SUPI != tc.wantSUPI || got.Subscribers != tc.wantSubs {
				t.Errorf("forCheck = %q with %d, want %q with %d", got.SUPI, got.Subscribers, tc.wantSUPI, tc.wantSubs)
			}
		})
	}
}

// RunStream hands each check the first SUPI of its own block, and every
// check the one SUPI when the suite has no pool.
func TestRunStreamAssignsSubscribers(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool int
		want []string
	}{
		{name: "pool", pool: 10, want: []string{"001010100007500", "001010100007501", "001010100007504"}},
		{name: "no pool", pool: 0, want: []string{"001010100007500", "001010100007500", "001010100007500"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := &Registry{}
			for i, n := range []CheckNeeds{{UEs: 1}, {UEs: 1, Subscribers: 3}, {UEs: 1}} {
				r.Register(recordSUPI{needy: needy{id: string(rune('A' + i)), needs: n}, got: &got})
			}
			r.Run(context.Background(), Env{UE: UESpec{SUPI: "001010100007500", Subscribers: tc.pool}}, 0)
			if len(got) != len(tc.want) {
				t.Fatalf("SUPIs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("check %d registered %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// recordSUPI records the SUPI its Env carries.
type recordSUPI struct {
	needy
	got *[]string
}

func (c recordSUPI) Run(_ context.Context, env Env) Result {
	*c.got = append(*c.got, env.UE.SUPI)
	return Result{Verdict: Pass}
}

func TestUseSubscriber(t *testing.T) {
	env := Env{UE: UESpec{SUPI: "001010100007510", Subscribers: 3, Ki: testKi, OPc: testOPc, GNBN3: "127.0.0.1"}}
	env.GNB.MCC, env.GNB.MNC = "001", "01"
	h, err := NewUEHarness(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"001010100007510", "001010100007511", "001010100007512", "001010100007510"} {
		if err := h.UseSubscriber(i); err != nil {
			t.Fatal(err)
		}
		if h.supi != want || h.sub.SUPI != want || h.id.MSIN != want[5:] {
			t.Errorf("subscriber %d: supi %s, subscription %s, MSIN %s; want %s", i, h.supi, h.sub.SUPI, h.id.MSIN, want)
		}
	}
	h.registered = true
	if err := h.UseSubscriber(1); err == nil {
		t.Error("UseSubscriber with a UE registered: no error")
	}
}

// verdictCheck is a UE check that returns a fixed verdict.
type verdictCheck struct {
	needy
	v Verdict
}

func (c verdictCheck) Run(context.Context, Env) Result {
	return Result{Verdict: c.v, Outcome: verdictOutcomeID(c.v)}
}

// Outcomes declares the check's one outcome when it is not one every
// check has.
func (c verdictCheck) Outcomes() []Outcome {
	if c.v == Error {
		return nil
	}
	return []Outcome{{ID: verdictOutcomeID(c.v), Verdict: c.v, Summary: "did it"}}
}

// An ERROR from a check that registered a subscriber an earlier check of
// the run registered says so; a check with its own subscriber, the first
// check, and any other verdict do not.
func TestSharedSubscriberNote(t *testing.T) {
	const note = "registered the subscriber A registered earlier in this run"
	for _, tc := range []struct {
		name     string
		pool     int
		second   Verdict
		wantNote bool
	}{
		{name: "shared, error", pool: 0, second: Error, wantNote: true},
		{name: "shared, pass", pool: 0, second: Pass},
		{name: "shared, fail", pool: 0, second: Fail},
		{name: "own subscriber, error", pool: 10, second: Error},
		{name: "pool of one, error", pool: 1, second: Error, wantNote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			r.Register(verdictCheck{needy: needy{id: "A", needs: CheckNeeds{UEs: 1}}, v: Error})
			r.Register(verdictCheck{needy: needy{id: "B", needs: CheckNeeds{UEs: 1}}, v: tc.second})
			res := r.Run(context.Background(), Env{UE: UESpec{SUPI: "001010100007500", Subscribers: tc.pool}}, 0)
			if len(res) != 2 {
				t.Fatalf("results = %d, want 2", len(res))
			}
			if strings.Contains(res[0].Detail, "registered earlier in this run") {
				t.Errorf("first check's detail %q carries the note", res[0].Detail)
			}
			if got := strings.Contains(res[1].Detail, note); got != tc.wantNote {
				t.Errorf("second check's detail %q: note %v, want %v", res[1].Detail, got, tc.wantNote)
			}
		})
	}
}

func TestJoinDetail(t *testing.T) {
	for _, tc := range []struct{ detail, more, want string }{
		{"", "", ""},
		{"", "b", "b"},
		{"a", "", "a"},
		{"a", "b", "a; b"},
	} {
		if got := joinDetail(tc.detail, tc.more); got != tc.want {
			t.Errorf("joinDetail(%q, %q) = %q, want %q", tc.detail, tc.more, got, tc.want)
		}
	}
}
