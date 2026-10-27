package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bgrewell/orbit/internal/gtpu"
	"github.com/bgrewell/orbit/internal/target"
)

// Option configures New.
type Option func(*options)

type options struct {
	locations *target.Locations
	// resolving, when set, is called each time a command starts resolving
	// its layers (tests: proving no-target runs never do).
	resolving func(cmd string)
}

// WithTargetLocations sets where -t/--target looks for target files, in
// place of ./orbit.yaml and ~/.config/orbit/targets (tests).
func WithTargetLocations(loc target.Locations) Option {
	return func(o *options) { o.locations = &loc }
}

// withResolving calls f each time a command starts resolving (tests).
func withResolving(f func(cmd string)) Option {
	return func(o *options) { o.resolving = f }
}

const (
	targetFlagHelp = "target to run against, from ./orbit.yaml or ~/.config/orbit/targets/<name>.yaml " +
		"(default $ORBIT_TARGET); flags and $ORBIT_* still override its fields"
	explainFlagHelp = "print where each setting came from (default, target, env or flag) and what it overrode, then run"
)

// gtpuPort is what --n3-bind takes when a target's N3 entry has no port:
// the GTP-U port, as the flag's own example (172.17.50.12:2152) has it.
var gtpuPort = strconv.Itoa(gtpu.Port)

// targets wires ADR-0008's resolver into the command tree: the persistent
// -t/--target and --explain flags, a PreRunE on every runnable command, and
// the leading-zero rule on every numeric flag bound to a path.
type targets struct {
	opts    options
	flag    *pflag.Flag // the root's --target
	explain bool
}

// installTargets adds the target flags to root and hooks every command
// under it. It runs after the tree is built.
func installTargets(root *cobra.Command, o options) {
	t := &targets{opts: o}
	root.PersistentFlags().StringP("target", "t", "", targetFlagHelp)
	root.PersistentFlags().BoolVar(&t.explain, "explain", false, explainFlagHelp)
	t.flag = root.PersistentFlags().Lookup("target")
	root.SetFlagErrorFunc(t.flagError)

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		strictNumbers(c)
		if c.RunE != nil {
			run := c.RunE
			c.RunE = func(cmd *cobra.Command, args []string) error {
				return explainUnfilled(cmd.Flags(), run(cmd, args))
			}
		}
		if c.Runnable() {
			prev := c.PreRunE
			c.PreRunE = func(cmd *cobra.Command, args []string) error {
				if err := t.apply(cmd); err != nil {
					return err
				}
				if prev != nil {
					return prev(cmd, args)
				}
				return nil
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(root)
}

// flagError turns the bare "unknown shorthand flag: 't'" on a command whose
// own --target is something else (ue latency, ue traffic: the echo
// destination) into an error that says so.
func (t *targets) flagError(c *cobra.Command, err error) error {
	if strings.Contains(err.Error(), "unknown shorthand flag: 't'") {
		if f := c.Flags().Lookup("target"); f != nil && f != t.flag {
			return fmt.Errorf("-t is not available on %s: its --target is the %s, and a run target does not apply to it", c.CommandPath(), f.Usage)
		}
	}
	return err
}

// strictNumber is a numeric flag's value with the leading-zero rule a
// target file applies: 0010 is an error, not octal 8 (pflag's base-0
// parse) or decimal 10.
type strictNumber struct{ pflag.Value }

func (s strictNumber) Set(v string) error {
	if err := target.LeadingZero(strings.TrimSpace(v)); err != nil {
		return err
	}
	return s.Value.Set(v)
}

// strictNumbers applies the leading-zero rule to every numeric flag c
// defines that is bound to a path.
func strictNumbers(c *cobra.Command) {
	wrap := func(f *pflag.Flag) {
		if _, done := f.Value.(strictNumber); done {
			return
		}
		if b, ok := target.FlagBinding(c.CommandPath(), f.Name); ok && b.Numeric() {
			f.Value = strictNumber{f.Value}
		}
	}
	c.Flags().VisitAll(wrap)
	c.PersistentFlags().VisitAll(wrap)
}

// boundFlags are the flags on cmd bound to a target or run path, by name,
// leaving out --target itself.
func boundFlags(cmd *cobra.Command) map[string]target.Binding {
	out := map[string]target.Binding{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if b, ok := target.FlagBinding(cmd.CommandPath(), f.Name); ok && b.Path != "target" {
			out[f.Name] = b
		}
	})
	return out
}

// apply resolves the layers for cmd and fills its untyped bound flags from
// the resolved fields (setFromTarget), so the command's own code reads
// target values through its flags. With no target named and no --explain,
// nothing is resolved and the command runs exactly as before.
func (t *targets) apply(cmd *cobra.Command) error {
	if cmd.Annotations[targetFileAnnotation] != "" {
		return nil // the command resolves its scenario file's target itself
	}
	path := cmd.CommandPath()
	fs := cmd.Flags()
	ours := fs.Lookup("target") == t.flag
	explicit := ours && t.flag.Changed
	bound := boundFlags(cmd)
	if len(bound) == 0 {
		switch {
		case explicit:
			return fmt.Errorf("--target does not apply to %s: it takes no target fields", path)
		case t.explain:
			return fmt.Errorf("--explain: %s takes no target fields", path)
		}
		return nil // $ORBIT_TARGET is ignored here, so a shell export never breaks it
	}
	// Whether a target is named needs only -t and $ORBIT_TARGET; with none
	// and no --explain, return before anything reads the other flags, so a
	// command runs exactly as it did before targets.
	name := ""
	if ours {
		if v, ok := os.LookupEnv("ORBIT_TARGET"); ok {
			name = strings.TrimSpace(v)
		}
		if explicit {
			name = strings.TrimSpace(t.flag.Value.String())
		}
	}
	if name == "" && !t.explain {
		return nil
	}
	if t.opts.resolving != nil {
		t.opts.resolving(path)
	}
	flagIn, err := target.FlagInput(path, fs)
	if err != nil {
		return err
	}
	envIn := target.EnvInput(os.LookupEnv)
	if !ours {
		envIn = withoutTarget(envIn)
	}
	// The command's own flag defaults are a default layer of their own
	// (shown as "default (orbit load)"): --explain shows where a field
	// would come from, and setFromTarget never fills from a default.
	defaults, err := target.DefaultsInput(path, fs)
	if err != nil {
		return err
	}
	inputs := []target.Input{target.DefaultInput(), defaults}
	if name != "" {
		f, err := t.find(name)
		if err != nil {
			return err
		}
		inputs = append(inputs, f.Input)
	}
	resolve := target.Resolve
	if name == "" {
		// --explain with no target only reports: a value the resolver finds
		// bad is a marked row, never an error, so it cannot change what the
		// command does.
		resolve = target.Report
	}
	r, err := resolve(append(inputs, envIn, flagIn)...)
	if err != nil {
		return err
	}
	// Filling from another flag needs a target named: --explain alone only
	// reports, and never changes what the command does.
	applied, err := setFromTarget(path, name, fs, bound, r, name != "")
	if err != nil {
		return err
	}
	if t.explain {
		explain(cmd.ErrOrStderr(), path, name, bound, r, applied)
	}
	return nil
}

func (t *targets) find(name string) (*target.File, error) {
	loc := t.opts.locations
	if loc == nil {
		l, err := target.DefaultLocations()
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", name, err)
		}
		loc = &l
	}
	return target.Find(name, *loc, os.LookupEnv)
}

// withoutTarget drops $ORBIT_TARGET from the env layer.
func withoutTarget(in target.Input) target.Input {
	out := target.Input{Layer: in.Layer}
	for _, s := range in.Settings {
		if s.Path != "target" {
			out.Settings = append(out.Settings, s)
		}
	}
	return out
}

// setFromTarget fills the bound flags the user did not type with their
// field's resolved value, when the target won the field or, with a target
// named (fromFlags), another flag bound to the same field did: the resolved
// field is the truth, so --n3-bind typed alone also gives --gnb-n3 its
// address. A typed flag is never overwritten, and two typed flags on one
// field each keep their own value. Env winners are left to the command's
// own $ORBIT_* fallback, which gives the same answer, and defaults to the
// flags' own. It returns, by flag name, the text set and, when the value
// had to be narrowed to fit the flag, how.
func setFromTarget(cmd, targetName string, fs *pflag.FlagSet, bound map[string]target.Binding, r *target.Resolved, fromFlags bool) (map[string]appliedFlag, error) {
	names := make([]string, 0, len(bound))
	typed := map[string]bool{}
	for n := range bound {
		names = append(names, n)
		typed[n] = fs.Lookup(n).Changed
	}
	sort.Strings(names)
	out := map[string]appliedFlag{}
	for _, n := range names {
		b := bound[n]
		f, ok := r.Lookup(b.Path)
		if !ok || typed[n] {
			continue
		}
		switch f.Source.Layer {
		case target.LayerTarget:
		case target.LayerFlag:
			if !fromFlags {
				continue
			}
		default:
			continue
		}
		fl := fs.Lookup(n)
		if fl.Deprecated != "" {
			continue // its replacement, bound to the same path, takes the value
		}
		a, ok, err := flagText(cmd, f.Path, n, b, f.Value, fl.Value, gnbCount(fs))
		if errors.Is(err, errNeedsIP) {
			// --gnb-n3 cannot take a host name that --n3-bind can: leave it
			// unfilled, and give the command's own error, if it needs the
			// flag, the reason (notFilled).
			from := shortSource(f.Source)
			if f.Source.Layer == target.LayerTarget {
				from = "target " + targetName + ", " + strings.TrimPrefix(from, "target ")
			}
			entry := ""
			if l, ok := f.Value.([]string); ok && len(l) > 0 {
				entry = l[0]
			}
			if fl.Annotations == nil {
				fl.Annotations = map[string][]string{}
			}
			fl.Annotations[notFilledAnnotation] = []string{fmt.Sprintf("%s[0] %q is a host name; --%s needs an IP, %s", f.Path, entry, n, from)}
			out[n] = appliedFlag{path: f.Path, note: "not filled: " + err.Error(), skipped: true}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %s for --%s: %w", f.Source, f.Path, n, err)
		}
		if !ok {
			continue
		}
		if f.Source.Layer == target.LayerFlag {
			note := "from " + f.Source.String() + " via " + f.Path
			if strings.HasSuffix(a.note, ", port "+gtpuPort+" added") {
				note += ", port " + gtpuPort + " added"
			}
			a.note = note
		}
		if sv, isSlice := fl.Value.(pflag.SliceValue); isSlice && a.list != nil {
			if err := sv.Replace(a.list); err != nil {
				return nil, fmt.Errorf("--%s from %s: %w", n, f.Source, err)
			}
			fl.Changed = true
		} else if err := fs.Set(n, a.text); err != nil {
			// The value is not echoed: it may be a key.
			return nil, fmt.Errorf("--%s from %s: %s does not fit the flag", n, f.Source, f.Path)
		}
		if fl.Annotations == nil {
			fl.Annotations = map[string][]string{}
		}
		fl.Annotations[fromTargetAnnotation] = []string{f.Path}
		a.path = f.Path
		out[n] = a
	}
	return out, nil
}

// notFilledAnnotation holds why setFromTarget left a flag unfilled, for
// an error that names the flag (explainUnfilled).
const notFilledAnnotation = "orbit/not-filled"

// explainUnfilled adds to a command's error the reason a flag it names was
// left unfilled from the target, so the user need not run --explain to see
// why (--gnb-n3 is required with --supi; the target's N3 entry is a host
// name).
func explainUnfilled(fs *pflag.FlagSet, err error) error {
	if err == nil {
		return nil
	}
	var reasons []string
	fs.VisitAll(func(f *pflag.Flag) {
		if r, ok := f.Annotations[notFilledAnnotation]; ok && strings.Contains(err.Error(), "--"+f.Name) {
			reasons = append(reasons, r...)
		}
	})
	if len(reasons) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(reasons, "; "))
}

// fromTargetAnnotation marks a flag setFromTarget filled: its value came
// from the resolver (the target, or another flag on the same field), not
// from the user typing it.
const fromTargetAnnotation = "orbit/from-target"

// userGave reports whether the user gave --name on the command line (or
// $ORBIT_*, for the flags that fall back to it), as opposed to its default
// or a value the resolver filled in. Code that refuses a flag the user should
// not have given ("--ki needs --supi") asks this, not Changed: a target
// describes the environment and may hold fields a run does not use.
func userGave(fs *pflag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil || !f.Changed {
		return false
	}
	_, fromTarget := f.Annotations[fromTargetAnnotation]
	return !fromTarget
}

// appliedFlag is what setFromTarget set one flag to.
type appliedFlag struct {
	path string
	text string
	list []string
	note string // how a pool was narrowed, "" when it was not
	// skipped is set when the flag was left unfilled; note says why.
	skipped bool
}

// flagText renders a resolved value as the text a flag takes. A list goes
// whole to a list flag, except an address list flag (load's --bind, one
// source per gNB), which takes gnbs concrete addresses from the pool (see
// poolAddresses); a single-address flag takes the list's first entry,
// narrowed to the form the flag wants (see firstAddress).
func flagText(cmd, path, name string, b target.Binding, v any, fv pflag.Value, gnbs int) (appliedFlag, bool, error) {
	switch x := v.(type) {
	case string:
		return appliedFlag{text: x}, true, nil
	case uint64:
		return appliedFlag{text: strconv.FormatUint(x, 10)}, true, nil
	case float64:
		return appliedFlag{text: strconv.FormatFloat(x, 'g', -1, 64)}, true, nil
	case bool:
		return appliedFlag{text: strconv.FormatBool(x)}, true, nil
	case []string:
		if _, isSlice := fv.(pflag.SliceValue); isSlice {
			if _, isAddr := addressShapes[name]; isAddr {
				return poolForGNBs(cmd, path, x, gnbs)
			}
			return appliedFlag{list: x, text: strings.Join(x, ",")}, true, nil
		}
		if len(x) == 0 {
			return appliedFlag{}, false, nil
		}
		text, note, err := firstAddress(name, x)
		return appliedFlag{text: text, note: note}, err == nil, err
	}
	return appliedFlag{}, false, fmt.Errorf("unsupported value type %T", v)
}

// gnbCount is how many gNBs the command runs, for an address list flag
// (--bind on load: one N2 source per gNB): its --gnb-count, or 1.
func gnbCount(fs *pflag.FlagSet) int {
	if f := fs.Lookup("gnb-count"); f != nil {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// poolForGNBs gives an address list flag (load's --bind: one N2 source per
// gNB) the first gnbs addresses of the pool, until leasing (S4) allocates
// from it. A pool of exactly one address passes as is, for the command's
// own rule to share among its gNBs; a longer pool with fewer addresses than
// gNBs is an error naming both.
func poolForGNBs(cmd, path string, pool []string, gnbs int) (appliedFlag, bool, error) {
	addrs, err := target.ExpandPool(pool, gnbs)
	if err != nil {
		return appliedFlag{}, false, err
	}
	if one, _ := target.ExpandPool(pool, 2); len(one) == 1 {
		return appliedFlag{list: one, text: one[0]}, true, nil
	}
	if len(addrs) < gnbs {
		return appliedFlag{}, false, fmt.Errorf("%s has %d addresses, %s needs %d (one per gNB)", path, len(addrs), cmd, gnbs)
	}
	a := appliedFlag{list: addrs, text: strings.Join(addrs, ",")}
	if !reflect.DeepEqual(addrs, pool) {
		a.note = fmt.Sprintf("first %d of %s", gnbs, strings.Join(pool, ", "))
	}
	return a, true, nil
}

// errNeedsIP is firstAddress's answer when a flag that takes only an IP
// (--gnb-n3) would get a host name: the flag is left unfilled.
var errNeedsIP = errors.New("takes an IP address; the field holds a host name")

// addressShapes are the single-address flags an address list maps to, and
// the form each takes: --bind ip[:port] (N2 source), --gnb-n3 an IP,
// --n3-bind ip:port (GTP-U source, port 2152 when the entry has none).
var addressShapes = map[string]string{"bind": "ip[:port]", "local": "ip[:port]", "gnb-n3": "ip", "n3-bind": "ip:port"}

// firstAddress narrows an address list to one address for a flag that
// takes one, until leasing (S4) picks from the pool: the first entry; an
// IP or ip:port as is, a range's low end, a CIDR's first host address; then
// the port dropped or defaulted to the flag's shape. note says what was
// done, "" when the list was the one plain entry used as is.
func firstAddress(flag string, list []string) (string, string, error) {
	entry := list[0]
	host, port, narrowed, err := target.PoolFirst(entry)
	if err != nil {
		return "", "", err
	}
	added, dropped := false, false
	switch addressShapes[flag] {
	case "ip":
		dropped = port != ""
		if _, err := netip.ParseAddr(host); err != nil {
			return "", "", errNeedsIP
		}
		port = ""
	case "ip:port":
		if port == "" {
			// TS 29.281 fixes the GTP-U port, so this is derived, not
			// guessed. Only a target-filled value gets it: a bare IP typed
			// as --n3-bind is passed on as today.
			port, added = gtpuPort, true
		}
	}
	text := host
	if port != "" {
		text = net.JoinHostPort(host, port)
	}
	note := ""
	if narrowed || len(list) > 1 || text != entry {
		note = "from " + strconv.Quote(entry)
		if len(list) > 1 {
			note += ", the first of " + strconv.Itoa(len(list))
		}
		if added {
			note += ", port " + gtpuPort + " added"
		}
		if dropped {
			note += ", port dropped"
		}
	}
	return text, note, nil
}

// explain prints, for each path cmd's flags bind, the value used and the
// layer and source that set it, what it overrode, and how a target value
// reached its flag; then the target's fields the command does not use.
// Values come from provenance, so secrets are masked.
func explain(w io.Writer, cmd, name string, bound map[string]target.Binding, r *target.Resolved, applied map[string]appliedFlag) {
	byPath := map[string]target.FieldProvenance{}
	for _, p := range r.Provenance {
		byPath[p.Path] = p
	}
	flagsOf := map[string][]string{} // concrete path → flags bound to it
	for n, b := range bound {
		if f, ok := r.Lookup(b.Path); ok {
			flagsOf[f.Path] = append(flagsOf[f.Path], n)
		}
	}
	paths := make([]string, 0, len(flagsOf))
	problems := map[string][]string{}
	for _, pr := range r.Problems {
		problems[pr.Path] = append(problems[pr.Path], pr.Err)
		if flagsOf[pr.Path] == nil {
			flagsOf[pr.Path] = []string{}
		}
	}
	for p := range flagsOf {
		paths = append(paths, p)
		sort.Strings(flagsOf[p])
	}
	sort.Strings(paths)

	if name == "" {
		name = "(none)"
	}
	_, _ = fmt.Fprintf(w, "%s: target %s\n", cmd, name)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FIELD\tVALUE\tSET BY\tOVERRODE\tFLAG")
	for _, p := range paths {
		pv := byPath[p]
		var over []string
		for _, o := range pv.Overridden {
			over = append(over, shortSource(o.Source)+"="+o.Value)
		}
		var flags []string
		for _, n := range flagsOf[p] {
			s := "--" + n
			if a, ok := applied[n]; ok && a.note != "" {
				if target.IsSecret(p) || a.skipped {
					s += " (" + a.note + ")"
				} else {
					s += "=" + a.text + " (" + a.note + ")"
				}
			}
			flags = append(flags, s)
		}
		value, setBy := pv.Value, shortSource(pv.Source)
		switch {
		case (value == "" || value == "[]") && pv.Source.Layer >= target.LayerEnv:
			// An explicit empty value clears the field (--bind "").
			value, setBy = "(cleared)", "cleared by "+setBy
		case value == "" || value == "[]":
			value = "(none)" // the default's, or a target slice with no SD
		}
		if _, set := byPath[p]; !set {
			value, setBy = "-", "-"
		}
		for _, e := range problems[p] {
			flags = append(flags, "⚠ "+e)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p, value, setBy, dash(strings.Join(over, "; ")), strings.Join(flags, " "))
	}
	_ = tw.Flush()
	var unused []string
	for _, pv := range r.Provenance {
		if pv.Source.Layer == target.LayerTarget && flagsOf[pv.Path] == nil && !documentField(pv.Path) && !usedBelow(pv.Path, flagsOf) {
			unused = append(unused, pv.Path)
		}
	}
	if len(unused) > 0 {
		_, _ = fmt.Fprintf(w, "not used by %s: %s\n", cmd, strings.Join(unused, ", "))
	}
}

// shortSource renders a source with a file path relative to the working
// directory, or under ~, so the table stays readable; provenance itself
// keeps the full path.
func shortSource(src target.Source) string {
	if src.Line > 0 && filepath.IsAbs(src.Name) {
		if wd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(wd, src.Name); err == nil && !strings.HasPrefix(rel, "..") {
				src.Name = rel
			}
		}
		if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(src.Name) && strings.HasPrefix(src.Name, home+string(filepath.Separator)) {
			src.Name = "~" + strings.TrimPrefix(src.Name, home)
		}
	}
	return src.String()
}

// usedBelow reports whether a field under list path p (core.slices[0].sd
// under core.slices) is bound by the command.
func usedBelow(p string, flagsOf map[string][]string) bool {
	for q := range flagsOf {
		if strings.HasPrefix(q, p+"[") {
			return true
		}
	}
	return false
}

// documentField reports whether p describes the target document itself
// (version, kind, name, a list element's name) rather than a setting.
func documentField(p string) bool {
	return p == "version" || p == "kind" || p == "name" || strings.HasSuffix(p, "].name")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
