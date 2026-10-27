package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// budgeted is a fakeTest that declares its own budget.
type budgeted struct {
	fakeTest
	b time.Duration
}

func (b budgeted) Budget() time.Duration { return b.b }

// ExpectedDuration is the per-test timeout, or a longer Budget; zero means
// the default timeout.
func TestExpectedDuration(t *testing.T) {
	plain := fakeTest{"A", Procedural, Pass, ""}
	long := budgeted{fakeTest{"B", NAS, Pass, ""}, 3 * time.Minute}
	short := budgeted{fakeTest{"C", NAS, Pass, ""}, 5 * time.Second}
	tests := []struct {
		test    Test
		perTest time.Duration
		want    time.Duration
	}{
		{plain, 20 * time.Second, 20 * time.Second},
		{plain, 0, DefaultPerTestTimeout},
		{long, 20 * time.Second, 3 * time.Minute},
		{long, 5 * time.Minute, 5 * time.Minute},
		{short, 20 * time.Second, 20 * time.Second},
	}
	for _, tc := range tests {
		if got := ExpectedDuration(tc.test, tc.perTest); got != tc.want {
			t.Errorf("ExpectedDuration(%s, %s) = %s, want %s", tc.test.ID(), tc.perTest, got, tc.want)
		}
	}
}

// RunWithStart calls start before each selected test, with its expected
// duration and its place among the selected tests, then emit; the category
// filter applies to both, and Select returns the same set.
func TestRunWithStartAnnouncesBeforeEach(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass, ""})
	r.Register(budgeted{fakeTest{"B", NAS, Pass, ""}, time.Minute})
	r.Register(fakeTest{"C", NAS, Fail, ""})
	var events []string
	r.RunWithStart(context.Background(), Env{}, time.Second,
		func(t Test, expected time.Duration, i, n int) {
			events = append(events, fmt.Sprintf("start %s %s %d/%d", t.ID(), expected, i, n))
		},
		func(res Result, i, n int) { events = append(events, fmt.Sprintf("done %s %d/%d", res.ID, i, n)) },
		NAS)
	want := []string{"start B 1m0s 0/2", "done B 0/2", "start C 1s 1/2", "done C 1/2"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if sel := r.Select(NAS); len(sel) != 2 || sel[0].ID() != "B" || sel[1].ID() != "C" {
		t.Errorf("Select(NAS) = %v", sel)
	}
	if sel := r.Select(); len(sel) != 3 {
		t.Errorf("Select() = %d tests, want 3", len(sel))
	}
}
