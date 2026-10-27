package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/target"
)

// newTargetCmd is `orbit target`: create, list, show and probe the targets
// -t names (ADR-0008 §1, ADR-0009 §5).
func newTargetCmd(serverURL *string, o options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Create, list, show and probe targets (where tests run)",
	}
	loc := func() (target.Locations, error) {
		if o.locations != nil {
			return *o.locations, nil
		}
		return target.DefaultLocations()
	}
	cmd.AddCommand(newTargetInitCmd(serverURL, loc))
	cmd.AddCommand(newTargetListCmd(loc))
	cmd.AddCommand(newTargetShowCmd(loc))
	cmd.AddCommand(newTargetProbeCmd(serverURL, loc))
	return cmd
}

// keyPlaceholders are what init writes for the keys unless --write-keys:
// the file then holds no key, and loading it expands them from the
// environment.
const (
	kiPlaceholder  = "${ORBIT_KI}"
	opcPlaceholder = "${ORBIT_OPC}"
)

// initDoc is the document init writes: the target schema's fields, in its
// order, with the empty parts left out.
type initDoc struct {
	Version     int           `yaml:"version"`
	Kind        string        `yaml:"kind"`
	Name        string        `yaml:"name"`
	Core        target.Core   `yaml:"core"`
	Subscribers []target.Pool `yaml:"subscribers,omitempty"`
	RAN         initRAN       `yaml:"ran,omitempty"`
	App         *target.App   `yaml:"app,omitempty"`
}

// parseResponder reads one --responder: name=control[,data_ip], e.g.
// n6=203.0.113.20:9551,203.0.113.21. A token is not taken on the command
// line; add it to the file as ${VAR}.
func parseResponder(v string) (target.Responder, error) {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return target.Responder{}, fmt.Errorf("--responder %q: want name=control[,data_ip], e.g. n6=203.0.113.20:9551,203.0.113.21", v)
	}
	control, dataIP, _ := strings.Cut(rest, ",")
	return target.Responder{Name: name, Control: control, DataIP: dataIP}, nil
}

type initRAN struct {
	N2SourceIPs []string `yaml:"n2_source_ips,omitempty"`
	N3IPs       []string `yaml:"n3_ips,omitempty"`
}

type targetLocations func() (target.Locations, error)

func newTargetInitCmd(serverURL *string, loc targetLocations) *cobra.Command {
	var (
		amf, firstSUPI, ki, opc, n2, n3 string
		responders                      []string
		id                              netIdentity
		count                           uint32
		writeKeys, noProbe, here, force bool
		timeout                         time.Duration
	)
	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Create a target file: the core, a subscriber pool, and the RAN addresses",
		Long: "Writes ~/.config/orbit/targets/<name>.yaml (or ./orbit.yaml with --here).\n" +
			"Unless --no-probe, it first runs one NG Setup through the orbit server,\n" +
			"from the server's host, which is where runs execute, and records the N2\n" +
			"source address it used. Keys are written as ${ORBIT_KI}/${ORBIT_OPC}\n" +
			"unless --write-keys. On a terminal it asks for what the flags leave out.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !target.ValidName(name) {
				return fmt.Errorf("target name %q: use letters, digits, '-' and '_'", name)
			}
			out := cmd.OutOrStdout()
			if interactive(cmd.InOrStdin()) {
				in := bufio.NewReader(cmd.InOrStdin())
				for _, q := range []struct {
					v       *string
					prompt  string
					missing bool
				}{
					{&amf, "AMF N2 address (host:port)", amf == ""},
					{&id.MCC, "MCC (3 digits)", id.MCC == "" && os.Getenv("ORBIT_MCC") == ""},
					{&id.MNC, "MNC (2 or 3 digits)", id.MNC == "" && os.Getenv("ORBIT_MNC") == ""},
					{&firstSUPI, "first SUPI of the subscriber pool (empty: none)", firstSUPI == "" && !cmd.Flags().Changed("first-supi")},
				} {
					if q.missing {
						v, err := ask(out, in, q.prompt)
						if err != nil {
							return err
						}
						*q.v = v
					}
				}
			}
			if amf == "" {
				return fmt.Errorf("target init needs --amf")
			}
			if err := id.resolve(true); err != nil {
				return err
			}
			if noProbe && n2 == "" {
				return fmt.Errorf("--no-probe needs --bind: without the NG Setup there is no N2 source to record")
			}
			mcc, mnc, tac := id.MCC, id.MNC, id.TAC

			doc := initDoc{Version: target.Version, Kind: target.Kind, Name: name,
				Core: target.Core{AMF: amf, PLMN: target.PLMN{MCC: mcc, MNC: mnc}, TAC: tac,
					Slices: []target.Slice{{SST: id.SST, SD: id.SD}}, DNN: id.DNN}}
			if firstSUPI != "" {
				p := target.Pool{Name: "main", FirstSUPI: firstSUPI, Count: count, Ki: kiPlaceholder, OPc: opcPlaceholder}
				if writeKeys {
					k, o, err := resolveCreds(ki, opc)
					if err != nil {
						return fmt.Errorf("--write-keys: %w", err)
					}
					p.Ki, p.OPc = k, o
				}
				doc.Subscribers = []target.Pool{p}
			} else if writeKeys || ki != "" || opc != "" {
				return fmt.Errorf("--write-keys, --ki and --opc describe the subscriber pool and need --first-supi")
			}

			// The N2 source is the server's: NG Setup from where runs execute.
			source := n2
			if !noProbe {
				if err := applyServerURL(cmd, serverURL); err != nil {
					return err
				}
				res, err := cellClient(serverURL).RunNGSetup(cmd.Context(), connect.NewRequest(&orbitv1.RunNGSetupRequest{
					AmfAddress:   amf,
					LocalAddress: n2,
					Gnb:          id.gnbProto(target.DefaultGNBID, 24, "orbit-init"),
					TimeoutMs:    uint32(timeout.Milliseconds()),
				}))
				switch {
				case err != nil && !force:
					return fmt.Errorf("NG Setup through the orbit server: %w; nothing written (--force writes the target anyway, --no-probe skips the NG Setup)", err)
				case err != nil:
					_, _ = fmt.Fprintf(out, "NG Setup through the orbit server failed (%v); writing anyway (--force)\n", err)
				case !res.Msg.GetAccepted() && !force:
					return fmt.Errorf("AMF rejected NG Setup: %s%s; nothing written (--force writes the target anyway)",
						res.Msg.GetCause(), plmnHint(res.Msg.GetCause(), mcc, mnc, tac))
				case !res.Msg.GetAccepted():
					_, _ = fmt.Fprintf(out, "AMF rejected NG Setup: %s; writing anyway (--force)\n", res.Msg.GetCause())
				default:
					if host, _, err := net.SplitHostPort(res.Msg.GetLocalAddress()); err == nil && n2 == "" {
						source = host
					}
					_, _ = fmt.Fprintf(out, "NG Setup accepted by AMF %q from %s (gNB ID %d, the first of the default range)\n",
						res.Msg.GetAmfName(), res.Msg.GetLocalAddress(), target.DefaultGNBID)
				}
			}
			if source != "" {
				doc.RAN.N2SourceIPs = []string{source}
				doc.RAN.N3IPs = []string{source}
			}
			if n3 != "" {
				doc.RAN.N3IPs = []string{n3}
			}

			for _, v := range responders {
				r, err := parseResponder(v)
				if err != nil {
					return err
				}
				if doc.App == nil {
					doc.App = &target.App{}
				}
				doc.App.Responders = append(doc.App.Responders, r)
			}

			b, err := yaml.Marshal(doc)
			if err != nil {
				return err
			}
			// The document must load as written; the placeholders expand to
			// a well-formed stand-in for the check only.
			if _, err := target.Parse(name+".yaml", b, func(k string) (string, bool) {
				if k == "ORBIT_KI" || k == "ORBIT_OPC" {
					return strings.Repeat("0", 32), true
				}
				return "", false
			}); err != nil {
				return fmt.Errorf("the target would not load: %w", err)
			}
			l, err := loc()
			if err != nil {
				return err
			}
			path := filepath.Join(l.ConfigDir, "targets", name+".yaml")
			if here {
				path = filepath.Join(l.Dir, "orbit.yaml")
			} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := writeTargetFile(path, b, here, force, name, l); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "wrote target %s to %s\n", name, path)
			switch {
			case firstSUPI == "":
				_, _ = fmt.Fprintf(out, "no subscriber pool: `orbit target probe %s` stops after NG Setup (add one with --first-supi)\n", name)
			case !writeKeys:
				_, _ = fmt.Fprintln(out, "keys are ${ORBIT_KI}/${ORBIT_OPC} in the file: export them before a probe or a run")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 endpoint (host:port)")
	addNetIdentityFlags(f, &id, withDNN())
	f.StringVar(&firstSUPI, "first-supi", "", "first "+supiFlagHelp+" of the subscriber pool (none: no pool)")
	f.Uint32Var(&count, "count", 1, "number of subscribers in the pool")
	f.BoolVar(&writeKeys, "write-keys", false, "write the keys (--ki/--opc or $ORBIT_KI/$ORBIT_OPC) into the file instead of ${ORBIT_KI}/${ORBIT_OPC}")
	f.StringVar(&ki, "ki", "", "with --write-keys: "+kiFlagHelp)
	f.StringVar(&opc, "opc", "", "with --write-keys: "+opcFlagHelp)
	f.StringVar(&n2, "bind", "", bindFlagHelp+"; recorded as the target's N2 source (default: the one the server's NG Setup used)")
	f.StringVar(&n3, "gnb-n3", "", "gNB N3 IP to record (default: the N2 source)")
	f.BoolVar(&noProbe, "no-probe", false, "skip the NG Setup (needs --bind)")
	f.StringArrayVar(&responders, "responder", nil, "an N6 responder, name=control[,data_ip] (repeatable), e.g. n6=203.0.113.20:9551,203.0.113.21")
	f.BoolVar(&here, "here", false, "write ./orbit.yaml instead (only when it does not exist)")
	f.BoolVar(&force, "force", false, "overwrite an existing file, and write even when the NG Setup fails")
	f.DurationVar(&timeout, "timeout", 10*time.Second, "bound on the NG Setup")
	for _, n := range []string{"tac", "sst", "count"} {
		f.Lookup(n).Value = strictNumber{f.Lookup(n).Value}
	}
	return cmd
}

// writeTargetFile writes a target file without ever following a link: a
// symlink (dangling or not) or any other non-regular file at path is
// refused, naming it. A new file is created O_EXCL at 0600, so a link
// planted after the check is refused too. With force an existing regular
// file is replaced by renaming a 0600 temp file from the same directory
// over it, which replaces the name, never a link's target, and never
// changes the mode of a file init did not create. With here, path must not
// exist at all: editing an existing YAML list would lose its comments.
func writeTargetFile(path string, b []byte, here, force bool, name string, l target.Locations) error {
	st, err := os.Lstat(path)
	switch {
	case err == nil && st.Mode()&os.ModeSymlink != 0:
		dest, _ := os.Readlink(path)
		return fmt.Errorf("%s is a symlink (to %s): init writes only regular files; remove the link or write elsewhere", path, dest)
	case err == nil && !st.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case err == nil && here:
		return fmt.Errorf("%s exists: add target %q to it by hand, or leave out --here to write %s",
			path, name, filepath.Join(l.ConfigDir, "targets", name+".yaml"))
	case err == nil && !force:
		return fmt.Errorf("%s exists: --force overwrites it", path)
	case err == nil:
		return replaceFile(path, b)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// replaceFile replaces the regular file at path with b through a 0600 temp
// file in the same directory, synced, then renamed over it.
func replaceFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}

// interactive reports whether r is a terminal, so init may ask. /dev/null,
// a pipe or a file is not one (cron, CI, nohup): then a missing value is an
// error naming its flag.
func interactive(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func ask(w io.Writer, in *bufio.Reader, prompt string) (string, error) {
	_, _ = fmt.Fprintf(w, "%s: ", prompt)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// plmnHint adds what to check to an unknown-PLMN NG Setup Failure (misc 4,
// TS 38.413 9.3.1.2).
func plmnHint(cause, mcc, mnc string, tac uint32) string {
	if cause != "misc(4)" {
		return ""
	}
	return fmt.Sprintf(" (unknown PLMN); core.plmn %s/%s or core.tac %d is not one the AMF serves", mcc, mnc, tac)
}

// listRow is one entry of `target list`.
type listRow struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	AMF    string `json:"amf,omitempty"`
	PLMN   string `json:"plmn,omitempty"`
	Status string `json:"status"`
}

func newTargetListCmd(loc targetLocations) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the targets in ./orbit.yaml and ~/.config/orbit/targets, with any load errors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := loc()
			if err != nil {
				return err
			}
			entries, err := target.List(l, os.LookupEnv)
			if err != nil {
				return err
			}
			var rows []listRow
			docs := map[string]int{}
			for _, e := range entries {
				docs[e.Path]++
				r := listRow{Name: e.Target.Name, File: shortPath(e.Path), Line: e.Line, Status: "ok"}
				if r.Name == "" {
					r.Name = fmt.Sprintf("(unnamed, document %d)", docs[e.Path])
				}
				switch {
				case e.Err != nil:
					r.Status = "error: " + e.Err.Error()
				case e.Shadowed:
					r.Status = "shadowed by ./orbit.yaml"
				}
				if e.Err == nil {
					r.AMF = e.Target.Core.AMF
					if p := e.Target.Core.PLMN; p.MCC != "" {
						r.PLMN = p.MCC + "/" + p.MNC
					}
				}
				rows = append(rows, r)
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				if rows == nil {
					rows = []listRow{}
				}
				b, _ := json.MarshalIndent(rows, "", "  ")
				_, _ = fmt.Fprintln(out, string(b))
				return nil
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintf(out, "no targets in %s or %s (create one with `orbit target init <name>`)\n",
					shortPath(filepath.Join(l.Dir, "orbit.yaml")), shortPath(filepath.Join(l.ConfigDir, "targets")))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tFILE\tAMF\tPLMN\tSTATUS")
			for _, r := range rows {
				file := r.File
				if r.Line > 0 {
					file += ":" + strconv.Itoa(r.Line)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, file, dash(r.AMF), dash(r.PLMN), r.Status)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

// shortPath is a file path relative to the working directory, or under ~.
func shortPath(p string) string {
	s := shortSource(target.Source{Name: p, Line: 1})
	return strings.TrimSuffix(strings.TrimPrefix(s, target.LayerDefault.String()+" "), ":1")
}

func newTargetShowCmd(loc targetLocations) *cobra.Command {
	var resolved, jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a target as loaded, or with --resolved where each field's value comes from",
		Long: "Without --resolved, the target document as loaded (${VAR} expanded, keys\n" +
			"<redacted>). With --resolved, the fields after the built-in defaults, the\n" +
			"target and $ORBIT_* are layered, each with the layer and source that set\n" +
			"it and what it overrode; flags are a command's own, so none apply here.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := loc()
			if err != nil {
				return err
			}
			f, err := target.Find(args[0], l, os.LookupEnv)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !resolved {
				t := masked(f.Target)
				if jsonOut {
					b, _ := json.MarshalIndent(t, "", "  ")
					_, _ = fmt.Fprintln(out, string(b))
					return nil
				}
				b, err := yaml.Marshal(t)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "# target %s, from %s:%d\n%s", f.Target.Name, shortPath(f.Path), f.Line, b)
				return nil
			}
			r, err := target.Resolve(target.DefaultInput(), f.Input, target.EnvInput(os.LookupEnv))
			if err != nil {
				return err
			}
			var rows []target.FieldProvenance
			for _, p := range r.Provenance {
				if !documentField(p.Path) {
					rows = append(rows, p)
				}
			}
			if jsonOut {
				b, _ := json.MarshalIndent(rows, "", "  ")
				_, _ = fmt.Fprintln(out, string(b))
				return nil
			}
			_, _ = fmt.Fprintf(out, "target %s, resolved: default < target %s < env\n", f.Target.Name, shortPath(f.Path))
			return printProvenance(out, rows)
		},
	}
	cmd.Flags().BoolVar(&resolved, "resolved", false, "show each field's value after layering, with its source")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

// masked is t with its secrets replaced.
func masked(t target.Target) target.Target {
	t.Subscribers = append([]target.Pool(nil), t.Subscribers...)
	for i := range t.Subscribers {
		if t.Subscribers[i].Ki != "" {
			t.Subscribers[i].Ki = target.Masked
		}
		if t.Subscribers[i].OPc != "" {
			t.Subscribers[i].OPc = target.Masked
		}
	}
	t.App.Responders = append([]target.Responder(nil), t.App.Responders...)
	for i := range t.App.Responders {
		if t.App.Responders[i].Token != "" {
			t.App.Responders[i].Token = target.Masked
		}
	}
	return t
}

// probeStep is one line of `target probe`.
type probeStep struct {
	name   string
	status string // "ok", "FAIL", "SKIP"
	detail string
}

func newTargetProbeCmd(serverURL *string, loc targetLocations) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "probe <name>",
		Short: "Check a target from the orbit server: NG Setup, one registration, one PDU session",
		Long: "Runs, through the orbit server and so from the host runs execute on, the\n" +
			"control-plane steps of the doctor (ADR-0009 §5): NG Setup, then registering\n" +
			"the pool's first SUPI, then a PDU session. It stops at the first failure,\n" +
			"naming what to check, and exits non-zero. Until leasing, it uses the target's\n" +
			"first three gNB IDs (one per association) and first SUPI, so a run using them\n" +
			"at the same time collides. The keys go to --server: until it has TLS, keep it\n" +
			"on loopback, or the keys cross the network in plaintext.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := loc()
			if err != nil {
				return err
			}
			f, err := target.Find(args[0], l, os.LookupEnv)
			if err != nil {
				return err
			}
			r, err := target.Resolve(target.DefaultInput(), f.Input, target.EnvInput(os.LookupEnv))
			if err != nil {
				return err
			}
			if err := applyServerURL(cmd, serverURL); err != nil {
				return err
			}
			return runProbe(cmd, serverURL, f.Target.Name, r.Target, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "bound on each step")
	return cmd
}

// probePlan is what probe sends: the resolved target's first gNB ID, N2
// source, slice and subscriber.
type probePlan struct {
	gnb        *orbitv1.GnbConfig
	bind, n3   string
	pool       *target.Pool
	sst        uint32
	sd, dnn    string
	gnbIDShown string
}

func planProbe(t target.Target) (probePlan, error) {
	p := probePlan{dnn: t.Core.DNN}
	if len(t.Core.Slices) > 0 {
		p.sst, p.sd = t.Core.Slices[0].SST, t.Core.Slices[0].SD
	}
	id, bits := t.RAN.GNBIDs.First, t.RAN.GNBIDs.Bits
	if id == 0 {
		id = target.DefaultGNBID
	}
	if bits == 0 {
		bits = 24
	}
	name := t.RAN.GNBName
	if name == "" {
		name = "orbit-probe"
	}
	p.gnbIDShown = fmt.Sprintf("%#x, %#x, %#x", id, id+1, id+2)
	p.gnb = &orbitv1.GnbConfig{Id: id, IdBits: bits, Name: name, Mcc: t.Core.PLMN.MCC, Mnc: t.Core.PLMN.MNC, Tac: t.Core.TAC,
		Slices: []*orbitv1.Snssai{{Sst: p.sst, Sd: p.sd}}}
	if len(t.RAN.N2SourceIPs) > 0 {
		b, _, err := firstAddress("bind", t.RAN.N2SourceIPs)
		if err != nil {
			return p, fmt.Errorf("ran.n2_source_ips: %w", err)
		}
		p.bind = b
	}
	if len(t.RAN.N3IPs) > 0 {
		if n3, _, err := firstAddress("gnb-n3", t.RAN.N3IPs); err == nil {
			p.n3 = n3
		}
	}
	if len(t.Subscribers) > 0 && t.Subscribers[0].FirstSUPI != "" {
		p.pool = &t.Subscribers[0]
	}
	return p, nil
}

// gnbFor is the plan's gNB with the ID for association n (0, 1, 2): each
// association takes its own, since a core may not cleanly re-key a gNB ID
// reused from a new association (SD-Core; see conformance.go).
func (p probePlan) gnbFor(n uint32) *orbitv1.GnbConfig {
	g := proto.Clone(p.gnb).(*orbitv1.GnbConfig)
	g.Id += n
	return g
}

func runProbe(cmd *cobra.Command, serverURL *string, name string, t target.Target, timeout time.Duration) error {
	out := cmd.OutOrStdout()
	plan, err := planProbe(t)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	ms := uint32(timeout.Milliseconds())
	var steps []probeStep
	fail := func(step, detail string) error {
		steps = append(steps, probeStep{name: step, status: "FAIL", detail: detail})
		printProbe(out, name, steps)
		return fmt.Errorf("probe %s failed at %s", name, step)
	}
	_, _ = fmt.Fprintf(out, "probe %s through %s (gNB IDs %s, one per association", name, *serverURL, plan.gnbIDShown)
	if plan.pool != nil {
		_, _ = fmt.Fprintf(out, ", SUPI %s", plan.pool.FirstSUPI)
	}
	_, _ = fmt.Fprintln(out, "; until leasing, a run using them at the same time collides)")

	// 1. NG Setup.
	start := time.Now()
	ng, err := cellClient(serverURL).RunNGSetup(ctx, connect.NewRequest(&orbitv1.RunNGSetupRequest{
		AmfAddress: t.Core.AMF, LocalAddress: plan.bind, Gnb: plan.gnbFor(0), TimeoutMs: ms,
	}))
	switch {
	case err != nil && connect.CodeOf(err) == connect.CodeUnavailable:
		return fail("NG Setup", fmt.Sprintf("AMF %s is not reachable over SCTP from the orbit server (%v); check core.amf and the N2 route", t.Core.AMF, err))
	case err != nil:
		return fail("NG Setup", fmt.Sprintf("NG Setup to AMF %s failed: %v", t.Core.AMF, err))
	case !ng.Msg.GetAccepted():
		return fail("NG Setup", fmt.Sprintf("AMF rejected NG Setup: %s%s", ng.Msg.GetCause(),
			plmnHint(ng.Msg.GetCause(), t.Core.PLMN.MCC, t.Core.PLMN.MNC, t.Core.TAC)))
	}
	steps = append(steps, probeStep{name: "NG Setup", status: "ok", detail: fmt.Sprintf("accepted by AMF %q from %s in %s",
		ng.Msg.GetAmfName(), ng.Msg.GetLocalAddress(), since(start))})

	if plan.pool == nil {
		skip := fmt.Sprintf("no subscribers in target %s; add one with first_supi/ki/opc", name)
		steps = append(steps, probeStep{name: "register", status: "SKIP", detail: skip},
			probeStep{name: "PDU session", status: "SKIP", detail: skip})
		printProbe(out, name, steps)
		return nil
	}
	pool := plan.pool
	if pool.Ki == "" || pool.OPc == "" {
		return fail("register", fmt.Sprintf("subscribers[%s] has no ki/opc; set them, or ${ORBIT_KI}/${ORBIT_OPC} with the variables exported", pool.Name))
	}
	register := func(withPDU bool) (*orbitv1.RegisterResponse, error) {
		assoc := uint32(1)
		if withPDU {
			assoc = 2
		}
		req := &orbitv1.RegisterRequest{
			AmfAddress: t.Core.AMF, BindAddress: plan.bind, Supi: pool.FirstSUPI, Gnb: plan.gnbFor(assoc),
			Credentials: &orbitv1.Credentials{Ki: pool.Ki, Opc: pool.OPc}, RoutingIndicator: pool.RoutingIndicator,
			TimeoutMs: ms,
		}
		if req.RoutingIndicator == "" {
			req.RoutingIndicator = "0"
		}
		if withPDU {
			req.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: plan.sst, Sd: plan.sd, Dnn: plan.dnn}
			req.GnbN3Addr = plan.n3
		}
		res, err := ueClient(serverURL).Register(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	deregister := func() {
		if _, err := ueClient(serverURL).Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: pool.FirstSUPI})); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "probe: deregistering %s after the probe: %v\n", pool.FirstSUPI, err)
		}
	}

	// 2. Register one UE, no PDU session.
	// TODO(T3 follow-up, "feat(engine): typed 5GMM/5GSM reject causes"):
	// name the 5GMM reject cause here once the engine reports it.
	start = time.Now()
	reg, err := register(false)
	if err != nil || !reg.GetRegistered() {
		if err == nil {
			err = errors.New("not registered")
		}
		return fail("register", fmt.Sprintf("registering %s failed: %v (reject cause not reported yet); check that subscribers[%s] (first_supi, ki, opc) is provisioned in the core",
			pool.FirstSUPI, connectMessage(err), pool.Name))
	}
	steps = append(steps, probeStep{name: "register", status: "ok", detail: fmt.Sprintf("%s registered in %s", pool.FirstSUPI, since(start))})
	deregister()

	// 3. A PDU session.
	// TODO(T3 follow-up): name the 5GSM reject cause, without waiting out
	// the timeout, once the engine detects the reject.
	start = time.Now()
	slice := fmt.Sprintf("%d/%s", plan.sst, plan.sd)
	if plan.sd == "" {
		slice = strconv.Itoa(int(plan.sst))
	}
	pdu, err := register(true)
	timedOut := err != nil && (connect.CodeOf(err) == connect.CodeDeadlineExceeded || strings.Contains(err.Error(), "deadline exceeded"))
	switch {
	case timedOut:
		return fail("PDU session", fmt.Sprintf("no PDU session accept within %s (DNN %s, slice %s)", timeout, plan.dnn, slice))
	case err != nil:
		return fail("PDU session", fmt.Sprintf("PDU session for DNN %s on slice %s failed: %v (reject cause not reported yet)", plan.dnn, slice, connectMessage(err)))
	case !pdu.GetSessionActive():
		deregister()
		return fail("PDU session", fmt.Sprintf("no PDU session accept within %s (DNN %s, slice %s)", timeout, plan.dnn, slice))
	}
	steps = append(steps, probeStep{name: "PDU session", status: "ok", detail: fmt.Sprintf("UE IP %s via UPF %s in %s",
		pdu.GetPduAddress(), pdu.GetUpfAddress(), since(start))})
	deregister()
	printProbe(out, name, steps)
	return nil
}

// connectMessage is a Connect error's message without its code prefix.
func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }

func printProbe(w io.Writer, name string, steps []probeStep) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	passed := 0
	for _, s := range steps {
		mark := map[string]string{"ok": "✓", "FAIL": "✗", "SKIP": "-"}[s.status]
		if s.status == "ok" {
			passed++
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%s\t%s\n", mark, s.name, s.status, s.detail)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "probe %s: %d of 3 steps passed\n", name, passed)
}

// printProvenance prints provenance rows: each field's value (masked when
// secret), the source that set it and what it overrode.
func printProvenance(w io.Writer, rows []target.FieldProvenance) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FIELD\tVALUE\tSET BY\tOVERRODE")
	for _, p := range rows {
		var over []string
		for _, o := range p.Overridden {
			over = append(over, shortSource(o.Source)+"="+o.Value)
		}
		v := p.Value
		if v == "" || v == "[]" {
			v = "(none)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Path, v, shortSource(p.Source), dash(strings.Join(over, "; ")))
	}
	return tw.Flush()
}
