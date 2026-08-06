package report

import (
	"strings"
	"testing"
	"time"
)

func minimal() Data {
	return Data{
		RunID: "run-abc", Name: "nightly", Kind: "FLEET", State: "COMPLETE",
		Started: time.Unix(1_700_000_000, 0), Duration: 90 * time.Second,
		Version: "v1.2.3",
	}
}

func TestHTMLIsSelfContained(t *testing.T) {
	d := minimal()
	d.Charts = []Chart{{Title: "T", Series: []Series{{Name: "a", Color: "#111", Values: []float64{1, 2, 3}}}}}
	html, err := HTML(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(html)
	// The whole point: it must open offline and survive being emailed.
	for _, forbidden := range []string{"<script", "src=\"http", "href=\"http", "@import", "cdn."} {
		if strings.Contains(s, forbidden) {
			t.Errorf("report is not self-contained; found %q", forbidden)
		}
	}
	if !strings.Contains(s, "@media print") {
		t.Error("no print stylesheet, so PDF export would use the screen layout")
	}
	if !strings.Contains(s, "<svg") {
		t.Error("chart missing")
	}
}

func TestHTMLEscapesUserContent(t *testing.T) {
	// A run name and a scenario document are attacker-adjacent: they come from
	// whoever started the run, and the report is opened in a browser.
	d := minimal()
	d.Name = `<script>alert(1)</script>`
	d.Config = "ki: <script>bad</script>"
	d.ConfigLabel = "Scenario"
	d.Events = []Event{{Message: `<img onerror=alert(1)>`, Kind: "attach"}}
	html, err := HTML(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(html)
	if strings.Contains(s, "<script>alert(1)</script>") {
		t.Error("run name was not escaped")
	}
	if strings.Contains(s, "<img onerror") {
		t.Error("event message was not escaped")
	}
	if strings.Contains(s, "<script>bad</script>") {
		t.Error("configuration was not escaped")
	}
}

func TestStatusTone(t *testing.T) {
	for state, want := range map[string]string{
		"COMPLETE": "ok", "FAILED": "bad", "CANCELLED": "warn", "RUNNING": "neutral",
	} {
		d := minimal()
		d.State = state
		if got := d.StatusTone(); got != want {
			t.Errorf("state %s → tone %s, want %s", state, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{1500 * time.Millisecond, "1.5s"},
		{90 * time.Second, "1m 30s"},
		{3725 * time.Second, "1h 02m 05s"},
	} {
		if got := FormatDuration(tc.in); got != tc.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHTMLWithNothingButIdentity(t *testing.T) {
	// A failed run has no results, no charts and no cohorts. It must still
	// render rather than erroring or producing a broken document.
	d := minimal()
	d.State = "FAILED"
	d.Err = "no UEs attached"
	html, err := HTML(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(html)
	if !strings.Contains(s, "no UEs attached") {
		t.Error("the failure reason is the most important thing on a failed run's report")
	}
	if !strings.Contains(s, "</html>") {
		t.Error("document truncated")
	}
}
