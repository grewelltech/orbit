package conformance

import (
	"context"
	"strings"
	"testing"
	"time"
)

type taggedTest struct {
	fakeTest
	tags []Tag
}

func (t taggedTest) Tags() []Tag { return t.tags }

func TestParseTag(t *testing.T) {
	for _, s := range []string{"negative", " Security ", "TIMING"} {
		if _, err := ParseTag(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	if _, err := ParseTag("negative-ie"); err == nil || !strings.Contains(err.Error(), "want one of negative, security, timing") {
		t.Errorf("err = %v", err)
	}
}

func TestValidateTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    []Tag
		wantErr string
	}{
		{name: "none"},
		{name: "two", tags: []Tag{TagNegative, TagSecurity}},
		{name: "outside the set", tags: []Tag{"fuzz"}, wantErr: `tag "fuzz" is not one of the closed set`},
		{name: "twice", tags: []Tag{TagTiming, TagTiming}, wantErr: `tag "timing" declared twice`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTags(taggedTest{fakeTest{"X", Pass, ""}, tc.tags})
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// Tagged narrows the run to the checks with any of the tags; the results
// carry the tags, and the verdict is the check's own.
func TestRegistryTagged(t *testing.T) {
	r := &Registry{}
	r.Register(taggedTest{fakeTest{"A", Pass, ""}, []Tag{TagNegative}})
	r.Register(taggedTest{fakeTest{"B", Fail, ""}, []Tag{TagSecurity, TagTiming}})
	r.Register(fakeTest{"C", Pass, ""})
	if err := r.Tagged("bogus"); err == nil {
		t.Fatal("an unknown tag was accepted")
	}
	if len(r.Select()) != 3 {
		t.Fatal("a refused tag narrowed the registry")
	}
	if err := r.Tagged("timing", "NEGATIVE"); err != nil {
		t.Fatal(err)
	}
	got := r.Run(context.Background(), Env{}, time.Second)
	if len(got) != 2 || got[0].ID != "A" || got[1].ID != "B" || got[1].Verdict != Fail {
		t.Fatalf("results %+v", got)
	}
	if len(got[1].Tags) != 2 || got[1].Tags[0] != TagSecurity {
		t.Errorf("tags %v", got[1].Tags)
	}
	if err := r.Tagged(); err != nil || len(r.Select()) != 3 {
		t.Errorf("no tags: %v, %d selected", err, len(r.Select()))
	}
}

// The built-in tags, by stimulus.
func TestBuiltinTags(t *testing.T) {
	want := map[Tag]int{TagNegative: 17, TagSecurity: 11, TagTiming: 4}
	got := map[Tag]int{}
	for _, c := range NewRegistry().Tests() {
		if err := validateTags(c); err != nil {
			t.Error(err)
		}
		for _, tag := range TagsOf(c) {
			got[tag]++
		}
	}
	for tag, n := range want {
		if got[tag] != n {
			t.Errorf("%d checks tagged %s, want %d", got[tag], tag, n)
		}
	}
	r := NewRegistry()
	if err := (Selection{Tags: []string{"timing"}, Groups: []string{"NAS / 5GMM specific procedures"}}).Apply(r); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range r.Select() {
		ids = append(ids, c.ID())
	}
	if strings.Join(ids, ",") != "NAS-REGISTRATION-AFTER-RELEASE,NAS-T3550-ACCEPT-RETRANSMIT" {
		t.Errorf("timing in 5GMM specific procedures: %v", ids)
	}
}
