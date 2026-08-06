package report

// documentHTML is the whole report: one file, inline CSS, no scripts, no
// external requests.
//
// The palette mirrors the dashboard's own tokens so a report looks like the
// tool that produced it, but resolves to LIGHT by default — a report is a
// document, and a dark page prints as a wall of ink. A dark-mode reader still
// gets a dark screen via prefers-color-scheme, and the print rules force light
// regardless, because @media print applies to the PDF export too.
const documentHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ORBIT run {{.RunID}}{{if .Name}} — {{.Name}}{{end}}</title>
<style>
  :root {
    --ink: #10171f; --ink-2: #40505f; --ink-3: #6b7c8c;
    --bg: #ffffff; --surface: #f6f8fa; --border: #d9e0e7;
    --accent: #0b6a78; --ok: #1c6b3f; --bad: #a11c2c; --warn: #8a5a06;
    --grid: #e6ebf0;
    --mono: ui-monospace, "IBM Plex Mono", "SF Mono", Menlo, monospace;
    --sans: "IBM Plex Sans", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --ink: #e6edf3; --ink-2: #a8b8c6; --ink-3: #7c8fa0;
      --bg: #0d1520; --surface: #131f2c; --border: #24384c;
      --accent: #4fd6e8; --ok: #4ad295; --bad: #ff7b8a; --warn: #ffc861;
      --grid: #1b2c3d;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 32px 28px 64px; background: var(--bg); color: var(--ink);
    font-family: var(--sans); font-size: 14px; line-height: 1.55;
    max-width: 900px; margin-inline: auto;
  }
  h1 { font-size: 20px; letter-spacing: .02em; margin: 0 0 2px; font-family: var(--mono); }
  h2 {
    font-size: 12px; text-transform: uppercase; letter-spacing: .09em;
    color: var(--ink-3); margin: 34px 0 10px; font-weight: 600;
    border-bottom: 1px solid var(--border); padding-bottom: 5px;
  }
  .sub { color: var(--ink-3); font-size: 13px; margin: 0 0 18px; font-family: var(--mono); }

  .banner {
    display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap;
    border: 1px solid var(--border); border-left-width: 4px;
    background: var(--surface); padding: 11px 14px; border-radius: 4px; margin-bottom: 6px;
  }
  .banner.ok   { border-left-color: var(--ok); }
  .banner.bad  { border-left-color: var(--bad); }
  .banner.warn { border-left-color: var(--warn); }
  .banner.neutral { border-left-color: var(--ink-3); }
  .state { font-family: var(--mono); font-weight: 600; letter-spacing: .06em; }
  .banner.ok .state   { color: var(--ok); }
  .banner.bad .state  { color: var(--bad); }
  .banner.warn .state { color: var(--warn); }
  .err { color: var(--bad); font-family: var(--mono); font-size: 13px; }

  .grid-kv { display: grid; grid-template-columns: repeat(auto-fit, minmax(168px, 1fr)); gap: 12px; }
  .kv {
    border: 1px solid var(--border); background: var(--surface);
    border-radius: 4px; padding: 9px 11px;
  }
  .kv .k {
    font-size: 10px; text-transform: uppercase; letter-spacing: .08em;
    color: var(--ink-3); font-weight: 600;
  }
  .kv .v { font-family: var(--mono); font-size: 17px; margin-top: 2px; }
  .kv .d { font-size: 11px; color: var(--ink-3); font-family: var(--mono); margin-top: 1px; }

  figure { margin: 0 0 18px; }
  figcaption {
    font-size: 11px; color: var(--ink-3); font-family: var(--mono);
    margin-bottom: 4px; display: flex; justify-content: space-between;
  }
  .chart { width: 100%; height: auto; display: block; }
  .chart .grid { stroke: var(--grid); stroke-width: 1; }
  .chart .axis { stroke: var(--border); stroke-width: 1; }
  .chart .ylab {
    fill: var(--ink-3); font-size: 9px; text-anchor: end; font-family: var(--mono);
  }
  .legend { display: flex; gap: 14px; flex-wrap: wrap; font-family: var(--mono); font-size: 11px; }
  .legend span { display: inline-flex; align-items: center; gap: 5px; color: var(--ink-2); }
  .swatch { width: 9px; height: 2px; display: inline-block; }

  table { border-collapse: collapse; width: 100%; font-family: var(--mono); font-size: 12px; }
  th, td { text-align: left; padding: 5px 9px; border-bottom: 1px solid var(--border); }
  th {
    color: var(--ink-3); font-weight: 600; font-size: 10px;
    text-transform: uppercase; letter-spacing: .06em;
  }
  td.num, th.num { text-align: right; }
  .note { font-size: 11px; color: var(--ink-3); margin-top: 5px; }

  pre {
    background: var(--surface); border: 1px solid var(--border); border-radius: 4px;
    padding: 11px 13px; overflow-x: auto; font-family: var(--mono);
    font-size: 11.5px; line-height: 1.5; margin: 0;
  }

  .events { font-family: var(--mono); font-size: 11.5px; }
  .events tr td:first-child { color: var(--ink-3); white-space: nowrap; }
  .sev-error { color: var(--bad); }
  .sev-warn  { color: var(--warn); }
  .sev-info  { color: var(--ink-2); }

  .notes { padding-left: 18px; margin: 0; }
  .notes li { color: var(--ink-2); font-size: 12.5px; margin-bottom: 4px; }

  footer {
    margin-top: 38px; padding-top: 10px; border-top: 1px solid var(--border);
    color: var(--ink-3); font-size: 11px; font-family: var(--mono);
    display: flex; justify-content: space-between; flex-wrap: wrap; gap: 8px;
  }

  /* Print / PDF. Forced light because a dark page prints as a wall of ink, and
     this block governs the PDF export too. Sections are kept off page breaks
     so a table is not split from its heading. */
  @media print {
    :root {
      --ink: #000; --ink-2: #333; --ink-3: #555;
      --bg: #fff; --surface: #fafbfc; --border: #c8d0d8; --grid: #e4e9ee;
      --accent: #0b6a78; --ok: #14532b; --bad: #8a1220; --warn: #6b4400;
    }
    @page { margin: 14mm; }
    body { padding: 0; max-width: none; font-size: 11pt; }
    h2 { break-after: avoid; }
    figure, table, pre, .banner, .kv { break-inside: avoid; }
    .events { font-size: 9pt; }
    footer { break-before: avoid; }
  }
</style>
</head>
<body>

<h1>ORBIT run report</h1>
<p class="sub">{{.RunID}}{{if .Name}} · {{.Name}}{{end}} · {{.Kind}}</p>

<div class="banner {{.StatusTone}}">
  <span class="state">{{.State}}</span>
  <span>{{stamp .Started}}</span>
  <span>duration {{.Duration}}</span>
</div>
{{if .Err}}<p class="err">{{.Err}}</p>{{end}}

{{if .Notes}}
<h2>Read this first</h2>
<ul class="notes">{{range .Notes}}<li>{{.}}</li>{{end}}</ul>
{{end}}

{{if .Headline}}
<h2>Results</h2>
<div class="grid-kv">
  {{range .Headline}}
  <div class="kv">
    <div class="k">{{.Label}}</div>
    <div class="v">{{.Value}}</div>
    {{if .Detail}}<div class="d">{{.Detail}}</div>{{end}}
  </div>
  {{end}}
</div>
{{end}}

{{$charts := false}}{{range .Charts}}{{if .HasData}}{{$charts = true}}{{end}}{{end}}
{{if $charts}}
<h2>Over the run</h2>
{{range .Charts}}{{if .HasData}}
<figure>
  <figcaption>
    <span>{{.Title}}{{if .Unit}} ({{.Unit}}){{end}}</span>
    <span class="legend">
      {{range .Series}}<span><i class="swatch" style="background:{{.Color}}"></i>{{.Name}}</span>{{end}}
    </span>
  </figcaption>
  {{safeSVG .SVG}}
</figure>
{{end}}{{end}}
{{end}}

{{range .Tables}}
<h2>{{.Title}}</h2>
<table>
  <thead><tr>{{range .Columns}}<th>{{.}}</th>{{end}}</tr></thead>
  <tbody>
    {{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}
  </tbody>
</table>
{{if .Note}}<p class="note">{{.Note}}</p>{{end}}
{{end}}

{{if .Config}}
<h2>{{.ConfigLabel}}</h2>
<pre>{{.Config}}</pre>
{{end}}

{{if .Events}}
<h2>Event log</h2>
<table class="events">
  <thead><tr><th>time</th><th>kind</th><th>subject</th><th>message</th></tr></thead>
  <tbody>
    {{range .Events}}
    <tr class="sev-{{.Severity}}">
      <td>{{.At}}</td><td>{{.Kind}}</td><td>{{.Subject}}</td><td>{{.Message}}</td>
    </tr>
    {{end}}
  </tbody>
</table>
{{end}}

<footer>
  <span>ORBIT {{.Version}}{{if .SourceHost}} · {{.SourceHost}}{{end}}</span>
  <span>generated {{stamp .Generated}}</span>
</footer>

</body>
</html>
`
