package cli

import (
	"fmt"
	"net/url"
	"os"

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

// validateServerURL accepts an absolute http or https URL with a host. A path
// prefix or trailing slash is fine: Connect joins the procedure path onto it.
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// withServerURL installs a PersistentPreRunE on a client command group that
// resolves *serverURL in place, so every subcommand's client constructor sees
// the final value without touching its call site. Only groups that talk to
// the API server get it: `serve`, `load`, `conformance`, `responder` and
// `version` never read $ORBIT_SERVER, so a malformed value cannot stop a
// server from starting.
//
// A fallback that is used is never silent: when the URL came from
// $ORBIT_SERVER the hook prints one line to stderr before the first RPC, so a
// redirected target shows in every transcript and log. Stderr only, so --json
// and piped stdout are unchanged. Nothing is printed for an explicit flag
// (the operator typed it) or for the default (nothing redirected the client).
//
// Cobra runs only the nearest PersistentPreRunE, so no descendant of a group
// may define its own; TestServerURLHookCoversClientCommands guards that.
func withServerURL(cmd *cobra.Command, serverURL *string) *cobra.Command {
	cmd.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
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
	return cmd
}
