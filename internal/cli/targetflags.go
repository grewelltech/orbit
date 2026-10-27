package cli

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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
}

// WithTargetLocations sets where -t/--target looks for target files, in
// place of ./orbit.yaml and ~/.config/orbit/targets (tests).
func WithTargetLocations(loc target.Locations) Option {
	return func(o *options) { o.locations = &loc }
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

// apply resolves the layers for cmd and sets every bound flag whose winning
// layer is the target, so the command's own code reads target values
// through its flags. Env and flag winners are left to that code, which
// already gives a flag precedence over $ORBIT_*; defaults are the flags'
// own. With no target named and no --explain, nothing is resolved and the
// command runs exactly as before.
func (t *targets) apply(cmd *cobra.Command) error {
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
	flagIn, err := target.FlagInput(path, fs)
	if err != nil {
		return err
	}
	envIn := target.EnvInput(os.LookupEnv)
	if !ours {
		envIn = withoutTarget(envIn)
	}
	name := target.SelectName(envIn, flagIn)
	if name == "" && !t.explain {
		return nil
	}
	inputs := []target.Input{target.DefaultInput()}
	if name != "" {
		f, err := t.find(name)
		if err != nil {
			return err
		}
		inputs = append(inputs, f.Input)
	}
	r, err := target.Resolve(append(inputs, envIn, flagIn)...)
	if err != nil {
		return err
	}
	applied, err := setFromTarget(fs, bound, r)
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

// setFromTarget sets each bound flag whose path the target layer won. It
// returns, by flag name, the text set and, when the target's value had to
// be narrowed to fit the flag (one address from a pool), how.
func setFromTarget(fs *pflag.FlagSet, bound map[string]target.Binding, r *target.Resolved) (map[string]appliedFlag, error) {
	names := make([]string, 0, len(bound))
	for n := range bound {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]appliedFlag{}
	for _, n := range names {
		b := bound[n]
		f, ok := r.Lookup(b.Path)
		if !ok || f.Source.Layer != target.LayerTarget {
			continue
		}
		fl := fs.Lookup(n)
		if fl.Deprecated != "" {
			continue // its replacement, bound to the same path, takes the value
		}
		a, ok, err := flagText(n, b, f.Value, fl.Value)
		if err != nil {
			return nil, fmt.Errorf("target %s: %s for --%s: %w", f.Source, f.Path, n, err)
		}
		if !ok {
			continue
		}
		if sv, isSlice := fl.Value.(pflag.SliceValue); isSlice && a.list != nil {
			if err := sv.Replace(a.list); err != nil {
				return nil, fmt.Errorf("--%s from target: %w", n, err)
			}
			fl.Changed = true
		} else if err := fs.Set(n, a.text); err != nil {
			// The value is not echoed: it may be a key.
			return nil, fmt.Errorf("--%s from target %s: %s does not fit the flag", n, f.Source, f.Path)
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

// fromTargetAnnotation marks a flag setFromTarget set: its value came from
// the target, not the command line.
const fromTargetAnnotation = "orbit/from-target"

// userGave reports whether the user gave --name on the command line (or
// $ORBIT_*, for the flags that fall back to it), as opposed to its default
// or a value the target supplied. Code that refuses a flag the user should
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
}

// flagText renders a resolved value as the text a flag takes. A list goes
// whole to a list flag; a single-address flag takes the list's first
// entry, narrowed to the form the flag wants (see firstAddress).
func flagText(name string, b target.Binding, v any, fv pflag.Value) (appliedFlag, bool, error) {
	switch x := v.(type) {
	case string:
		return appliedFlag{text: x}, true, nil
	case uint64:
		return appliedFlag{text: strconv.FormatUint(x, 10)}, true, nil
	case float64:
		return appliedFlag{text: strconv.FormatFloat(x, 'g', -1, 64)}, true, nil
	case bool:
		return appliedFlag{text: strconv.FormatBool(x)}, true, nil
	case []target.Slice:
		if len(x) == 0 {
			return appliedFlag{}, false, nil
		}
		note := ""
		if len(x) > 1 {
			note = "the first slice of " + strconv.Itoa(len(x))
		}
		switch b.Part {
		case "sst":
			return appliedFlag{text: strconv.FormatUint(uint64(x[0].SST), 10), note: note}, true, nil
		case "sd":
			return appliedFlag{text: x[0].SD, note: note}, true, nil
		}
		return appliedFlag{}, false, fmt.Errorf("a slice list needs --sst/--sd")
	case []string:
		if _, isSlice := fv.(pflag.SliceValue); isSlice {
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
	var addr netip.Addr
	port := ""
	narrowed := false
	if a, err := netip.ParseAddr(entry); err == nil {
		addr = a
	} else if ap, err := netip.ParseAddrPort(entry); err == nil {
		addr, port = ap.Addr(), strconv.Itoa(int(ap.Port()))
	} else if lo, _, found := strings.Cut(entry, "-"); found {
		a, err := netip.ParseAddr(lo)
		if err != nil {
			return "", "", fmt.Errorf("entry %q is not an address", entry)
		}
		addr, narrowed = a, true
	} else if p, err := netip.ParsePrefix(entry); err == nil {
		addr, narrowed = p.Masked().Addr(), true
		if addr.Is4() && p.Bits() < 31 {
			addr = addr.Next() // skip the network address
		}
	} else {
		return "", "", fmt.Errorf("entry %q is not an address", entry)
	}
	switch addressShapes[flag] {
	case "ip":
		port = ""
	case "ip:port":
		if port == "" {
			port = gtpuPort
		}
	}
	text := addr.String()
	if port != "" {
		text = net.JoinHostPort(text, port)
	}
	note := ""
	if narrowed || len(list) > 1 || text != entry {
		note = "from " + strconv.Quote(entry)
		if len(list) > 1 {
			note += ", the first of " + strconv.Itoa(len(list))
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
				if target.IsSecret(p) {
					s += " (" + a.note + ")"
				} else {
					s += "=" + a.text + " (" + a.note + ")"
				}
			}
			flags = append(flags, s)
		}
		value, setBy := pv.Value, shortSource(pv.Source)
		if value == "" || value == "[]" {
			// An empty value from a layer clears the field (--bind "").
			value, setBy = "(cleared)", "cleared by "+setBy
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p, value, setBy, dash(strings.Join(over, "; ")), strings.Join(flags, " "))
	}
	_ = tw.Flush()
	var unused []string
	for _, pv := range r.Provenance {
		if pv.Source.Layer == target.LayerTarget && flagsOf[pv.Path] == nil && !documentField(pv.Path) {
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
