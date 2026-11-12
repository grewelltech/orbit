package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// summaryMaxLen is the longest a summary may be: it fits the live view's
// description column without truncation.
const summaryMaxLen = 58

// Every check of the default run and of every pack has a summary that fits
// the live view, and the table has no entry for a check that is gone.
func TestEveryCheckHasSummary(t *testing.T) {
	all := SiteTests()
	ids := map[string]bool{}
	for _, c := range all {
		ids[c.ID()] = true
		s := SummaryOf(c.ID())
		switch {
		case s == "":
			t.Errorf("%s has no summary", c.ID())
		case utf8.RuneCountInString(s) > summaryMaxLen:
			t.Errorf("%s: summary %q is longer than %d", c.ID(), s, summaryMaxLen)
		case strings.TrimSpace(s) != s || strings.HasSuffix(s, "."):
			t.Errorf("%s: summary %q has surrounding space or a full stop", c.ID(), s)
		}
	}
	for id := range summaries {
		if !ids[id] {
			t.Errorf("summary for %s, which is not a registered check", id)
		}
	}
}

func TestDocURLs(t *testing.T) {
	for _, tc := range []struct {
		id, outcome, want string
	}{
		{"NGAP-NG-SETUP", "", "https://orbit.grewelltech.com/conformance/checks/ngap-ng-setup/"},
		{"NGAP-NG-SETUP", "unanswered", "https://orbit.grewelltech.com/conformance/checks/ngap-ng-setup/#unanswered"},
	} {
		if got := OutcomeURL(tc.id, tc.outcome); got != tc.want {
			t.Errorf("OutcomeURL(%q, %q) = %q, want %q", tc.id, tc.outcome, got, tc.want)
		}
	}
	if got := DocURL("GTPU-ECHO-RESPONSE"); got != "https://orbit.grewelltech.com/conformance/checks/gtpu-echo-response/" {
		t.Errorf("DocURL = %q", got)
	}
}

func TestCategory(t *testing.T) {
	for _, tc := range []struct{ group, want string }{
		{"NGAP / Interface Management Procedures / NG Setup", "NGAP"},
		{"TS 23.501 / User Plane Management / x", "TS 23.501"},
		{"GTPv1-U", "GTPv1-U"},
		{"", ""},
	} {
		if got := Category(tc.group); got != tc.want {
			t.Errorf("Category(%q) = %q, want %q", tc.group, got, tc.want)
		}
	}
}

// The committed site pages are what the registry renders, with no page
// left over from a check that is gone; regenerate with go generate
// ./internal/conformance.
func TestSitePagesAreCurrent(t *testing.T) {
	dir := filepath.Join("..", "..", "site", "src", "content", "docs", "conformance", "checks")
	pages := SitePages(SiteTests())
	for name, want := range pages {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%v: run go generate ./internal/conformance", err)
			continue
		}
		if string(b) != want {
			t.Errorf("%s is stale: run go generate ./internal/conformance", name)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, f := range files {
		if _, ok := pages[filepath.Base(f)]; !ok {
			extra = append(extra, filepath.Base(f))
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("pages for no registered check: %v", extra)
	}
}

// Every outcome a check can report has an anchor on its page, so the link
// a run prints for it lands on it; the index links every page.
func TestSitePagesAnchorEveryOutcome(t *testing.T) {
	tests := SiteTests()
	pages := SitePages(tests)
	for _, c := range tests {
		page := pages[strings.ToLower(c.ID())+".md"]
		if !strings.Contains(page, "title: "+c.ID()+"\n") {
			t.Errorf("%s: page has no title", c.ID())
		}
		for _, o := range AllOutcomes(c) {
			if !strings.Contains(page, `<span id="`+o.ID+`"></span>`) {
				t.Errorf("%s: no anchor for outcome %s", c.ID(), o.ID)
			}
		}
		if !strings.Contains(pages["index.md"], "](./"+strings.ToLower(c.ID())+"/)") {
			t.Errorf("index does not link %s", c.ID())
		}
	}
}

func TestSiteEscape(t *testing.T) {
	for in, want := range map[string]string{"a | b": `a \| b`, "x<y>": "x&lt;y>", "plain": "plain"} {
		if got := siteEscape(in); got != want {
			t.Errorf("siteEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
