package cli

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/bgrewell/orbit/internal/tlspin"
)

// newServerCmd is `orbit server`: the servers this client trusts, in
// ~/.config/orbit/servers.yaml (ADR-0008 §4).
func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Trust an ORBIT API server's TLS key, and keep its token",
	}
	cmd.AddCommand(newServerTrustCmd(), newServerListCmd(), newServerForgetCmd())
	return cmd
}

func newServerTrustCmd() *cobra.Command {
	var fingerprint, tokenFile string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "trust <https-url>",
		Short: "Pin a server's TLS key (and keep its token) after you confirm its fingerprint",
		Long: "Connects to the server, shows its certificate and the SHA-256 fingerprint of\n" +
			"its public key, and asks you to confirm it before pinning it in\n" +
			"~/.config/orbit/servers.yaml. Compare the fingerprint with the one\n" +
			"`orbit serve` logs at startup. Without a terminal it pins only when\n" +
			"--fingerprint is given and matches: nothing is trusted on first use.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := url.Parse(args[0])
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%q: give the server's https:// URL (orbit serve --tls auto)", args[0])
			}
			host := u.Host
			if u.Port() == "" {
				host = net.JoinHostPort(u.Hostname(), "443")
			}
			leaf, err := peerCertificate(host, timeout)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			got := tlspin.Fingerprint(leaf)
			_, _ = fmt.Fprintf(out, "server:      %s\nsubject:     %s\nnames:       %s\nvalid:       %s to %s\nfingerprint: %s\n",
				args[0], leaf.Subject, strings.Join(certNames(leaf), ", "),
				leaf.NotBefore.Format(time.DateOnly), leaf.NotAfter.Format(time.DateOnly), got)
			switch {
			case fingerprint != "":
				if fingerprint != got {
					return fmt.Errorf("the server's fingerprint %s is not the --fingerprint %s; not trusted", got, fingerprint)
				}
			case interactive(cmd.InOrStdin()):
				answer, err := ask(out, bufio.NewReader(cmd.InOrStdin()), "trust this server? [y/N]")
				if err != nil {
					return err
				}
				if a := strings.ToLower(answer); a != "y" && a != "yes" {
					return errors.New("not trusted")
				}
			default:
				return fmt.Errorf("no terminal to confirm on: compare the fingerprint above with the one orbit serve logged, then pass --fingerprint %s", got)
			}
			token := ""
			if tokenFile != "" {
				if token, err = readTokenFile(tokenFile); err != nil {
					return err
				}
			} else if v := strings.TrimSpace(os.Getenv("ORBIT_TOKEN")); v != "" {
				token = v
			}
			path, err := serversPath()
			if err != nil {
				return err
			}
			f, err := loadServers(path)
			if err != nil {
				return err
			}
			entry := serverEntry{URL: args[0], Pin: got, Token: token}
			if old, ok := f.lookup(args[0]); ok && token == "" {
				entry.Token = old.Token // re-pinning keeps the token
			}
			f = f.without(args[0])
			f.Servers = append(f.Servers, entry)
			if err := writeServers(path, f); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "trusted %s (pinned in %s)\n", args[0], shortPath(path))
			return nil
		},
	}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "pin without asking when the server's fingerprint is this (sha256/…), for scripts")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file whose first line is the server's bearer token, kept with the pin (default $ORBIT_TOKEN)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "bound on the TLS connection")
	return cmd
}

// peerCertificate is the leaf certificate host presents. Verification is
// off only to read it: trust pins it after the user confirms it.
func peerCertificate(host string, timeout time.Duration) (*x509.Certificate, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", host, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, fmt.Errorf("TLS to %s: %w", host, err)
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s sent no certificate", host)
	}
	return certs[0], nil
}

func certNames(c *x509.Certificate) []string {
	out := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	if len(out) == 0 {
		out = []string{"(none)"}
	}
	return out
}

func newServerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the trusted servers (tokens masked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := serversPath()
			if err != nil {
				return err
			}
			f, err := loadServers(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(f.Servers) == 0 {
				_, _ = fmt.Fprintln(out, "no trusted servers (orbit server trust <https-url>)")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "SERVER\tPIN\tTOKEN")
			for _, s := range f.Servers {
				tok := "-"
				if s.Token != "" {
					tok = "<redacted>"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", s.URL, dash(s.Pin), tok)
			}
			return tw.Flush()
		},
	}
}

func newServerForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <url>",
		Short: "Remove a server's pin and token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := serversPath()
			if err != nil {
				return err
			}
			f, err := loadServers(path)
			if err != nil {
				return err
			}
			if _, ok := f.lookup(args[0]); !ok {
				return fmt.Errorf("%s is not a trusted server", args[0])
			}
			if err := writeServers(path, f.without(args[0])); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forgot %s\n", args[0])
			return nil
		},
	}
}

func (f serversFile) without(raw string) serversFile {
	k := serverKey(raw)
	var out serversFile
	for _, s := range f.Servers {
		if serverKey(s.URL) != k {
			out.Servers = append(out.Servers, s)
		}
	}
	return out
}

// writeServers writes servers.yaml at 0600 in a 0700 directory, never
// through a link (writeSecretFile).
func writeServers(path string, f serversFile) error {
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeSecretFile(path, b)
}

// writeSecretFile writes b to path at 0600: a new file O_EXCL, an existing
// regular one replaced through a temp file and a rename; a symlink or any
// other non-regular file at path is refused.
func writeSecretFile(path string, b []byte) error {
	st, err := os.Lstat(path)
	switch {
	case err == nil && st.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink: it holds tokens, so it is written only as a regular file", path)
	case err == nil && !st.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
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
	return f.Close()
}
