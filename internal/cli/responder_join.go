package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/join"
)

// cmdLogger logs to the command's stderr: a responder's journal.
func cmdLogger(cmd *cobra.Command) *slog.Logger {
	return slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
}

// defaultResponderConfig is where orbit join keeps a responder's config.
const defaultResponderConfig = "/etc/orbit/responder.yaml"

// newResponderRegisterCmd registers this host's responder with a join token
// (ADR-0009 §3). The join script runs it, as root, after its confirmed plan.
func newResponderRegisterCmd(version string) *cobra.Command {
	var (
		server, pin, tokenFile, name, control, dataIP, ports, config, owner string
		fresh                                                               bool
	)
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this host's responder with an orbit server, using a join token (run by orbit join)",
		Long: "Register this host's responder with the server a join token came from, and write\n" +
			"its config (--config, mode 0600). The token is read from --token-file, never the\n" +
			"command line. A config already there makes it a re-registration, which rotates\n" +
			"the responder's credential and loom token and keeps its ID; it needs a fresh join\n" +
			"token all the same.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := join.ReadSecretFile(tokenFile)
			if err != nil {
				return fmt.Errorf("--token-file: %w", err)
			}
			lo, hi, err := join.ParsePorts(ports)
			if err != nil {
				return err
			}
			uid, gid := -1, -1
			if owner != "" {
				u, err := user.Lookup(owner)
				if err != nil {
					return fmt.Errorf("--owner: %w", err)
				}
				if uid, err = strconv.Atoi(u.Uid); err != nil {
					return fmt.Errorf("--owner: uid %q: %w", u.Uid, err)
				}
				if gid, err = strconv.Atoi(u.Gid); err != nil {
					return fmt.Errorf("--owner: gid %q: %w", u.Gid, err)
				}
			}
			client, err := join.Client(server, pin)
			if err != nil {
				return err
			}
			c, err := join.Register(cmd.Context(), client, strings.TrimSpace(string(b)), config, join.Registration{
				Server: server, Pin: pin, Name: name, Control: control, DataIP: dataIP, PortLow: lo, PortHigh: hi,
				Version: version, UID: uid, GID: gid, Fresh: fresh})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered as %s (%s)\n", c.Name, c.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&server, "server", "", "the server, https://host:port")
	f.StringVar(&pin, "pin", "", "the server's key, sha256/<base64>")
	f.StringVar(&tokenFile, "token-file", "", "the join token, in a file (mode 0600)")
	f.StringVar(&name, "name", "", "the responder's name")
	f.StringVar(&control, "control", "", "the responder's control listen address, host:port")
	f.StringVar(&dataIP, "data-ip", "", "the responder's N6 data address")
	f.StringVar(&ports, "data-ports", "", "the responder's data port range, LO-HI")
	f.StringVar(&config, "config", defaultResponderConfig, "where to write the responder's config")
	f.StringVar(&owner, "owner", "", "the user to own the config (default: root, for systemd LoadCredential)")
	f.BoolVar(&fresh, "fresh", false, "register anew over an existing config whose credential the server no longer knows or removed")
	for _, n := range []string{"server", "pin", "token-file", "name", "control", "data-ip", "data-ports"} {
		_ = cmd.MarkFlagRequired(n)
	}
	return cmd
}

// newResponderCheckCmd waits for the server to see the responder service's
// own heartbeat: the join script's last step.
func newResponderCheckCmd() *cobra.Command {
	var (
		config string
		wait   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Wait until the server has seen this host's responder service's heartbeat",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := join.LoadConfig(config)
			if err != nil {
				return err
			}
			client, err := join.Client(c.Server, c.Pin)
			if err != nil {
				return err
			}
			err = join.Check(cmd.Context(), client, c.Credential, wait)
			if errors.Is(err, join.ErrResponderRemoved) {
				return &ExitError{Code: ExitResponderRemoved, Err: errors.New("the server removed this responder (orbit responder remove); join again to re-register it")}
			}
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered as %s (%s), online\n", c.Name, c.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&config, "config", defaultResponderConfig, "the responder's config")
	cmd.Flags().DurationVar(&wait, "wait", 20*time.Second, "how long to wait for the service's heartbeat")
	return cmd
}

func responderClient(url *string) orbitv1connect.ResponderServiceClient {
	h, guard := apiClient(*url)
	return orbitv1connect.NewResponderServiceClient(h, *url, guard)
}

func newResponderListCmd(serverURL *string) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the responders registered with the server (never their secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "" && output != "wide" {
				return fmt.Errorf("-o %q: only wide", output)
			}
			res, err := responderClient(serverURL).ListResponders(cmd.Context(), connect.NewRequest(&orbitv1.ListRespondersRequest{}))
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			head := "NAME\tID\tSTATE\tLAST SEEN\tCONTROL\tDATA IP\tVERSION\tTOKEN\tREGISTERED"
			if output == "wide" {
				head += "\tSOURCE\tREGISTERED FROM"
			}
			_, _ = fmt.Fprintln(tw, head)
			now := time.Now()
			marked := false
			for _, r := range res.Msg.GetResponders() {
				seen := "-"
				if n := r.GetLastSeenUnixNano(); n > 0 {
					seen = now.Sub(time.Unix(0, n)).Round(time.Second).String() + " ago"
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s", r.GetName(), r.GetId(), r.GetState(), seen,
					r.GetControlAddress(), r.GetDataIp(), dash(r.GetVersion()), r.GetJoinTokenId(),
					time.Unix(0, r.GetRegisteredUnixNano()).UTC().Format(time.RFC3339))
				if output == "wide" {
					from := dash(r.GetRegisteredFrom())
					if registeredElsewhere(r.GetRegisteredFrom(), r.GetControlAddress()) {
						from += " !"
						marked = true
					}
					line += "\t" + dash(r.GetSource()) + "\t" + from
				}
				_, _ = fmt.Fprintln(tw, line)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if marked {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "! registered from a different address than its control address (NAT is fine; otherwise check who used the join token)")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "wide adds the source addresses")
	return cmd
}

func newResponderRemoveCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name|id>",
		Short: "Remove a registered responder: its credential and loom token stop working, and it stops",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := responderClient(serverURL).RemoveResponder(cmd.Context(), connect.NewRequest(&orbitv1.RemoveResponderRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "removed responder %s (%s)\n", args[0], res.Msg.GetId())
			return nil
		},
	}
}

// registeredElsewhere reports whether a responder registered from a host
// other than its control address's: a hint, since NAT does it too, and a
// join token lets its holder name any control address.
func registeredElsewhere(from, control string) bool {
	fh, _, err1 := net.SplitHostPort(from)
	ch, _, err2 := net.SplitHostPort(control)
	return err1 == nil && err2 == nil && fh != ch
}
