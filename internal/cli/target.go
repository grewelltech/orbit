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
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/plan"
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
						res.Msg.GetCause(), target.PLMNHint(res.Msg.GetCause(), mcc, mnc, tac))
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
				_, _ = fmt.Fprintf(out, "no subscriber pool: `orbit target probe %s` fails at register (add one with --first-supi)\n", name)
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

func newTargetProbeCmd(serverURL *string, loc targetLocations) *cobra.Command {
	var (
		timeout  time.Duration
		coreOnly bool
	)
	cmd := &cobra.Command{
		Use:   "probe <name>",
		Short: "Check a target from the orbit server, step by step: the core, then the user plane to the responder",
		Long: "Runs the doctor (ADR-0009 §5) on the orbit server, and so from the host runs\n" +
			"execute on, printing each step as it finishes:\n" +
			"  1 NG Setup  2 register  3 PDU session  4 responder  5 control  6 UE to N6 echo  7 data ports\n" +
			"It stops at the first failure, naming what to fix, and SKIPs the rest; a\n" +
			"failure exits 1, and a probe that could not start (a lease another run holds,\n" +
			"no server) exits 3. The probe leases the target's first three gNB IDs, its\n" +
			"first SUPI and its N2 source for as long as it runs, so it never collides with\n" +
			"a run. --core-only stops after step 3, for a target with no app host yet.\n" +
			"The keys go to --server: until it has TLS, keep it on loopback, or the keys\n" +
			"cross the network in plaintext.",
		Example: "  orbit target probe lab1\n  orbit target probe lab1 --core-only --timeout 5s",
		Args:    cobra.ExactArgs(1),
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
			if timeout <= 0 || timeout > time.Hour {
				return fmt.Errorf("--timeout %s: want more than 0 and at most 1h", timeout)
			}
			return runProbe(cmd, serverURL, f.Target.Name, r.Target, timeout, coreOnly)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "bound on each step")
	cmd.Flags().BoolVar(&coreOnly, "core-only", false, "stop after the PDU session (steps 1-3): the user plane is not checked")
	return cmd
}

// runProbe streams the server's probe steps as they come and sums them up.
func runProbe(cmd *cobra.Command, serverURL *string, name string, t target.Target, timeout time.Duration, coreOnly bool) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "probe %s through %s\n", name, *serverURL)
	stream, err := targetClient(serverURL).Probe(cmd.Context(), connect.NewRequest(&orbitv1.ProbeRequest{
		Target: plan.TargetProto(t), TargetName: name, CoreOnly: coreOnly, StepTimeoutMs: uint32(timeout.Milliseconds()),
	}))
	if err != nil {
		return exitWith(ExitCouldNotStart, fmt.Errorf("probe %s could not start: %w", name, err))
	}
	defer func() { _ = stream.Close() }()
	var passed, skipped, total int
	failed := ""
	for stream.Receive() {
		s := stream.Msg()
		total++
		mark := map[string]string{"OK": "✓", "FAIL": "✗", "SKIP": "-"}[s.GetStatus()]
		_, _ = fmt.Fprintf(out, "%s %d %-15s %-4s  %s\n", mark, s.GetIndex(), s.GetName(), s.GetStatus(), s.GetDetail())
		if s.GetFix() != "" {
			_, _ = fmt.Fprintf(out, "      fix: %s\n", s.GetFix())
		}
		switch s.GetStatus() {
		case "OK":
			passed++
		case "SKIP":
			skipped++
		case "FAIL":
			if failed == "" {
				failed = s.GetName()
			}
		}
	}
	if err := stream.Err(); err != nil {
		if total == 0 {
			return exitWith(ExitCouldNotStart, fmt.Errorf("probe %s could not start: %w", name, err))
		}
		return exitWith(ExitDidNotComplete, fmt.Errorf("probe %s stopped after %d steps: %w", name, total, err))
	}
	if coreOnly && failed == "" {
		_, _ = fmt.Fprintf(out, "probe %s: core OK; user plane not checked (--core-only)\n", name)
		return nil
	}
	_, _ = fmt.Fprintf(out, "probe %s: %d of %d steps passed (%d skipped)\n", name, passed, total, skipped)
	if failed != "" {
		return gateFailed("probe %s failed at %s", name, failed)
	}
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
