package cli

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// serverFlagHelp is the one help text for --server. The cobra default is empty
// so "not given" is observable, which means cobra prints no default suffix;
// the text carries the resolution rule instead.
const serverFlagHelp = "base URL of the ORBIT API server (default $ORBIT_SERVER, then http://" + DefaultListen + ")"

// defaultServerURL is where the CLI looks when neither --server nor
// $ORBIT_SERVER is set: the loopback listener `orbit serve` binds by default.
const defaultServerURL = "http://" + DefaultListen

// serverSource says where a resolved server URL came from, so the hook can
// tell the operator when the environment redirected the client.
type serverSource int

const (
	// serverFromDefault is the built-in loopback URL; nothing redirected the client.
	serverFromDefault serverSource = iota
	// serverFromFlag is an explicit --server; the operator typed it.
	serverFromFlag
	// serverFromEnv is $ORBIT_SERVER; the command prints it so a redirected
	// target is visible in every transcript and log.
	serverFromEnv
)

// String names the source the way an error or the stderr line shows it.
func (s serverSource) String() string {
	switch s {
	case serverFromFlag:
		return "--server"
	case serverFromEnv:
		return "$ORBIT_SERVER"
	default:
		return "default"
	}
}

// resolveServerURL returns the API server base URL and where it came from: a
// non-empty --server value, else a non-empty $ORBIT_SERVER, else the loopback
// default. The same precedence as resolveCreds and netIdentity.resolve, so
// every ORBIT_* variable behaves alike.
//
// The result is validated here, before any client is built, so a malformed
// URL fails with one message naming the source it came from instead of a
// transport error at the first RPC. A bare host:port is rejected rather than
// prefixed: the message tells the operator exactly what to type.
func resolveServerURL(flag string) (string, serverSource, error) {
	source, raw := serverFromFlag, flag
	if raw == "" {
		source, raw = serverFromEnv, os.Getenv("ORBIT_SERVER")
	}
	if raw == "" {
		return defaultServerURL, serverFromDefault, nil
	}
	if err := validateServerURL(raw); err != nil {
		return "", source, fmt.Errorf("invalid server URL %q (from %s): want http://host:port or https://host:port", raw, source)
	}
	return raw, source, nil
}

// validateServerURL accepts an absolute http or https URL with a hostname and,
// if a port is given, one in 1..65535, so "fails before the first RPC" is
// exact rather than left to the dial. A path prefix or trailing slash is
// fine: Connect joins the procedure path onto it.
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("missing host")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port %q is not in 1..65535", p)
		}
	}
	return nil
}

// applyServerURL resolves *serverURL in place and, when $ORBIT_SERVER chose
// it, prints one line to c's stderr before the first RPC, so a redirected
// target shows in every transcript and log. Stderr only, so --json and piped
// stdout are unchanged. Nothing is printed for an explicit flag (the operator
// typed it) or for the default (nothing redirected the client).
//
// withServerURL calls it from a group hook; `run` calls it directly on its
// step path, because a `kind: fleet` scenario drives the engine in-process
// and must neither be gated by the variable nor claim a redirect.
func applyServerURL(c *cobra.Command, serverURL *string) error {
	resolved, source, err := resolveServerURL(*serverURL)
	if err != nil {
		return err
	}
	if source == serverFromEnv {
		// A failed stderr write is no reason to abort the command.
		_, _ = fmt.Fprintf(c.ErrOrStderr(), "server: %s (from %s)\n", resolved, source)
	}
	*serverURL = resolved
	return nil
}

// withServerURL installs a PersistentPreRunE on a client command group that
// applies applyServerURL, so every subcommand's client constructor sees the
// final value without touching its call site. Only groups whose every leaf
// talks to the API server get it (system, cell, ue, runs): `serve`, `load`,
// `conformance`, `responder` and `version` never read $ORBIT_SERVER, so a
// malformed value cannot stop a server from starting. `run` is the one
// client command without the hook: it is dual-mode, and only its step path
// builds a client, so it resolves in RunE after reading the scenario kind.
//
// Cobra runs only the nearest PersistentPreRunE, so no descendant of a group
// may define its own; TestServerURLHookCoversClientCommands guards that.
func withServerURL(cmd *cobra.Command, serverURL *string) *cobra.Command {
	cmd.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
		return applyServerURL(c, serverURL)
	}
	return cmd
}
