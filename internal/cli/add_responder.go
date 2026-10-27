package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// sshHostRE is what --ssh takes: a host, user@host or an ssh_config alias,
// never anything ssh would read as an option.
var sshHostRE = regexp.MustCompile(`^[A-Za-z0-9_.@:\[\]][A-Za-z0-9_.@:\[\]-]*$`)

// remotePathRE is the path the app host's mktemp may answer with.
var remotePathRE = regexp.MustCompile(`^/tmp/orbit-join\.[A-Za-z0-9]{10}$`)

// remoteSudo runs the script as root on the app host: directly when the
// ssh user is root, else through sudo (which may ask for a password on
// the terminal ssh -t gives it).
func remoteSudo(path, args string) string {
	return `if [ "$(id -u)" = 0 ]; then sh ` + shellQuote(path) + " " + args + "; else sudo sh " + shellQuote(path) + " " + args + "; fi"
}

// remoteSh is body as the one command ssh hands the login shell: sh -c and
// body single-quoted, so a fish or csh login shell runs it under sh too.
func remoteSh(body string) string { return "sh -c " + shellQuote(body) }

// shellQuote is s as one single-quoted POSIX shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newTargetAddResponderCmd is `orbit target add-responder --ssh <host>`
// (ADR-0009 §4): the join one-liner, run on the app host through the
// user's own ssh. No SSH library, no key read, nothing stored: the
// user's ~/.ssh/config, agent and known_hosts apply as they are.
func newTargetAddResponderCmd(serverURL *string) *cobra.Command {
	var (
		host, name, upfN6, n6Subnet, mgmt string
		n6IP, bind                        string
		uePools                           []string
		ttl                               time.Duration
		yes                               bool
		noFirewall, noRoute, noSystemd    bool
	)
	cmd := &cobra.Command{
		Use:   "add-responder --ssh <host> -t <target>",
		Short: "Set up an app host as an N6 responder over ssh: the join script, planned, confirmed, applied",
		Long: "Issue a join token, fetch its script from the server (pinned to its key), copy it to\n" +
			"the app host over your own ssh, show its plan (--dry-run there), and after you\n" +
			"confirm, apply it (--yes there, with sudo). The token never appears on a command\n" +
			"line, here or on the app host: the script reaches it on ssh's stdin, in a mode-0600\n" +
			"file that is removed afterwards. --yes skips the confirmation here; without a\n" +
			"terminal it is required.",
		Example: "  orbit target add-responder --ssh app1.lab -t lab1\n" +
			"  orbit target add-responder --ssh ops@10.0.0.9 -t lab1 --name app1 --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !sshHostRE.MatchString(host) {
				return fmt.Errorf("--ssh %q: want a host, user@host or an ssh_config alias", host)
			}
			if ttl < 0 || ttl%time.Second != 0 {
				return fmt.Errorf("--ttl %s: a whole number of seconds", ttl)
			}
			in := cmd.InOrStdin()
			if !yes && !interactive(in) {
				return errors.New("no terminal to confirm the plan on: pass --yes")
			}
			sshPath, err := exec.LookPath("ssh")
			if err != nil {
				return errors.New("ssh is not on PATH: --ssh runs your own ssh client")
			}
			scriptArgs, err := passArgs(n6IP, bind, noFirewall, noRoute, noSystemd)
			if err != nil {
				return err
			}
			// Ctrl-C from here on cancels the run rather than the process, so
			// the token below is always revoked if no registration used it.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			client := joinClient(serverURL)
			res, err := client.CreateJoinToken(ctx, connect.NewRequest(&orbitv1.CreateJoinTokenRequest{
				Name: name, TtlSeconds: uint32(ttl / time.Second),
				Spec: &orbitv1.JoinSpec{UePools: uePools, UpfN6: upfN6, N6Subnet: n6Subnet, ManagementAddress: mgmt},
			}))
			if err != nil {
				return err
			}
			tokenID := res.Msg.GetIssued().GetId()
			// A token no registration used is revoked on every way out: a
			// failure, a Ctrl-C, a declined plan, or an apply that had
			// nothing to register (a rerun on a joined host). A used token
			// is no longer pending, so revokeUnused leaves it alone.
			defer revokeUnused(client, tokenID, cmd.ErrOrStderr())
			script, err := fetchJoinScript(ctx, res.Msg.GetUrl(), res.Msg.GetPin())
			if err != nil {
				return fmt.Errorf("fetching the join script: %w", err)
			}
			s := &sshSession{ssh: sshPath, host: host, tty: interactive(in), args: scriptArgs, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr()}
			return s.run(ctx, script, yes, in)
		},
	}
	f := cmd.Flags()
	f.StringVar(&host, "ssh", "", "the app host, as you would give it to ssh (host, user@host or an ssh_config alias)")
	f.StringVar(&name, "name", "", "the responder's name (default: the app host's hostname)")
	f.DurationVar(&ttl, "ttl", 0, "how long the token lasts (default 10m, at most 1h)")
	f.StringSliceVar(&uePools, "ue-pool", nil, "a UE address pool (CIDR) the app host routes back through the UPF (target: core.ue_pools)")
	f.StringVar(&upfN6, "upf-n6", "", "the UPF's N6 address (target: core.upf_n6)")
	f.StringVar(&n6Subnet, "n6-subnet", "", "the UPF's N6 subnet (CIDR) (target: core.n6_subnet)")
	f.StringVar(&mgmt, "mgmt", "", "the address the app host reaches this server at, host[:port] (default: the server's listen address)")
	f.BoolVar(&yes, "yes", false, "apply without asking here (required without a terminal)")
	f.StringVar(&n6IP, "n6-ip", "", "the app host's N6 address, passed to the join script (default: its source toward the UPF's N6 address)")
	f.StringVar(&bind, "bind", "", "the responder's control address, IP:port, passed to the join script (default: its source toward this server, on the join's control port)")
	f.BoolVar(&noFirewall, "no-firewall", false, "pass --no-firewall to the join script: it leaves the firewall alone (rules added by hand)")
	f.BoolVar(&noRoute, "no-route", false, "pass --no-route to the join script: it adds no UE pool route")
	f.BoolVar(&noSystemd, "no-systemd", false, "pass --no-systemd to the join script: no unit; run the responder yourself")
	_ = cmd.MarkFlagRequired("ssh")
	return cmd
}

// passArgs is the join script options add-responder passes through, each
// checked here and single-quoted for the remote shell. The script checks
// them again against the host's own addresses.
func passArgs(n6IP, bind string, noFirewall, noRoute, noSystemd bool) (string, error) {
	var args []string
	if n6IP != "" {
		if _, err := netip.ParseAddr(n6IP); err != nil {
			return "", fmt.Errorf("--n6-ip %q: want an IP address", n6IP)
		}
		args = append(args, "--n6-ip", shellQuote(n6IP))
	}
	if bind != "" {
		ap, err := netip.ParseAddrPort(bind)
		if err != nil || ap.Port() == 0 {
			return "", fmt.Errorf("--bind %q: want IP:port ([addr]:port for IPv6)", bind)
		}
		args = append(args, "--bind", shellQuote(bind))
	}
	for _, b := range []struct {
		on   bool
		flag string
	}{{noFirewall, "--no-firewall"}, {noRoute, "--no-route"}, {noSystemd, "--no-systemd"}} {
		if b.on {
			args = append(args, b.flag)
		}
	}
	return strings.Join(args, " "), nil
}

// joinTokenRevoker is the part of JoinService revokeUnused uses.
type joinTokenRevoker interface {
	ListJoinTokens(context.Context, *connect.Request[orbitv1.ListJoinTokensRequest]) (*connect.Response[orbitv1.ListJoinTokensResponse], error)
	RevokeJoinToken(context.Context, *connect.Request[orbitv1.RevokeJoinTokenRequest]) (*connect.Response[orbitv1.RevokeJoinTokenResponse], error)
}

// revokeUnused revokes join token id if it is still pending, best effort,
// on a context of its own: the command's may be cancelled. A token a
// registration used (an apply that registered, then failed) is not pending
// and is left alone. A failure is told, with the ID, and changes nothing
// else: the error that brought us here is the one returned.
func revokeUnused(client joinTokenRevoker, id string, errOut io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := client.ListJoinTokens(ctx, connect.NewRequest(&orbitv1.ListJoinTokensRequest{}))
	if err == nil {
		pending := false
		for _, t := range list.Msg.GetTokens() {
			pending = pending || t.GetId() == id
		}
		if !pending {
			return
		}
		_, err = client.RevokeJoinToken(ctx, connect.NewRequest(&orbitv1.RevokeJoinTokenRequest{Id: id}))
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "orbit: could not revoke the unused join token %s: %v; revoke it with orbit join revoke %s\n", id, err, id)
		return
	}
	_, _ = fmt.Fprintf(errOut, "orbit: revoked the unused join token %s\n", id)
}

// fetchJoinScript fetches the join script from url, pinned to the server's
// key; the URL holds the token and never leaves this process.
func fetchJoinScript(ctx context.Context, url, pin string) ([]byte, error) {
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: tlspin.VerifyPin(pin)}, // #nosec G402 -- the pin is the check
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("the server's join URL does not parse")
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("the server did not answer for the join script")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %s", res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

// sshSession runs the join script on host through the user's ssh.
type sshSession struct {
	ssh, host   string
	tty         bool
	args        string // the join script options passed through, quoted
	out, errOut io.Writer
}

// remote runs command on the host. tty asks ssh for a terminal (sudo's
// password prompt); stdin is what the remote command reads.
func (s *sshSession) remote(ctx context.Context, tty bool, command string, stdin io.Reader, stdout io.Writer) error {
	args := []string{}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, s.host, command)
	c := exec.CommandContext(ctx, s.ssh, args...) // #nosec G204 -- the user's own ssh, the host checked, the command ours
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, s.errOut
	// A cancelled ssh may leave a child holding its output open: do not
	// wait on it past a second.
	c.WaitDelay = time.Second
	if stdin == nil && tty {
		c.Stdin = os.Stdin
	}
	return c.Run()
}

// run is the whole exchange: a private file, the script copied into it on
// stdin, its plan shown, the confirmation here, the apply. The file goes
// on every path, a failure or a cancel (the caller's Ctrl-C) included.
func (s *sshSession) run(ctx context.Context, script []byte, yes bool, in io.Reader) error {
	var out bytes.Buffer
	if err := s.remote(ctx, false, remoteSh("umask 077; mktemp /tmp/orbit-join.XXXXXXXXXX"), nil, &out); err != nil {
		return fmt.Errorf("ssh %s: mktemp: %w", s.host, err)
	}
	path := strings.TrimSpace(out.String())
	if !remotePathRE.MatchString(path) {
		return fmt.Errorf("ssh %s: mktemp answered %q, not a /tmp/orbit-join.* path", s.host, path)
	}
	defer func() {
		// One more ssh, on a context of its own: the run's may be cancelled.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.remote(cctx, false, remoteSh("rm -f "+shellQuote(path)), nil, io.Discard); err != nil {
			_, _ = fmt.Fprintf(s.errOut, "orbit: could not remove %s on %s: %v; remove it by hand\n", path, s.host, err)
		}
	}()
	if err := s.remote(ctx, false, remoteSh("cat > "+shellQuote(path)), bytes.NewReader(script), io.Discard); err != nil {
		return fmt.Errorf("ssh %s: copying the script: %w", s.host, err)
	}
	if err := s.remote(ctx, s.tty, remoteSh(remoteSudo(path, s.opts("--dry-run"))), nil, s.out); err != nil {
		return fmt.Errorf("ssh %s: the plan (--dry-run) failed: %w", s.host, err)
	}
	if !yes {
		_, _ = fmt.Fprintf(s.errOut, "Apply this plan on %s? [y/N] ", s.host)
		ans, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.TrimSpace(ans) {
		case "y", "Y", "yes", "YES":
		default:
			_, _ = fmt.Fprintf(s.errOut, "not applied: nothing on %s was changed\n", s.host)
			return nil
		}
	}
	// The apply plans again on the host and prints that plan before it
	// changes anything.
	apply := remoteSudo(path, s.opts("--yes")) + "; rc=$?; rm -f " + shellQuote(path) + "; exit $rc"
	if err := s.remote(ctx, s.tty, remoteSh(apply), nil, s.out); err != nil {
		return fmt.Errorf("ssh %s: the join script failed: %w", s.host, err)
	}
	return nil
}

// opts is mode (--dry-run or --yes) and the options passed through.
func (s *sshSession) opts(mode string) string {
	if s.args == "" {
		return mode
	}
	return mode + " " + s.args
}
