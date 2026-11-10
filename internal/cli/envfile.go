package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/bgrewell/orbit/internal/target"
)

// envFileHelp is the help text for --env-file.
const envFileHelp = "load a dotenv-style file (KEY=VALUE, with an optional `export`, " +
	"quotes and # comments) into the environment before config resolution; " +
	"repeatable; a variable already set in the environment, or by an earlier " +
	"--env-file, wins"

// envPair is one KEY=VALUE from a dotenv file, kept in file order so the
// caller, not the parser, decides precedence.
type envPair struct {
	Key   string
	Value string
}

// envNameRe is a valid environment-variable name: a letter or underscore,
// then letters, digits or underscores.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnvFile reads a dotenv-style file: one KEY=VALUE per line, an
// optional `export ` prefix, `#` comment lines, blank lines, and single- or
// double-quoted values. Quotes are literal: nothing inside them is expanded
// or unescaped, so a quoted value may hold spaces, `#` or `=`. An unquoted
// value may also hold `=`, and a trailing ` # comment` is dropped as a POSIX
// shell sourcing the file would. The pairs are returned in file order; the
// error names only the line and, for a bad name, the name (never a value, in
// case it is a secret).
func parseEnvFile(r io.Reader) ([]envPair, error) {
	var out []envPair
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimLeft(strings.TrimRight(sc.Text(), "\r"), " \t")
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if rest, ok := cutExport(s); ok {
			s = rest
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: not KEY=VALUE", line)
		}
		key := strings.TrimRight(s[:eq], " \t")
		if !envNameRe.MatchString(key) {
			return nil, fmt.Errorf("line %d: %q is not a valid variable name", line, key)
		}
		val, err := parseEnvValue(s[eq+1:])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, envPair{Key: key, Value: val})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// cutExport strips a leading `export ` (or `export\t`) prefix, reporting
// whether it was present. Bare `export` with no name is left alone.
func cutExport(s string) (string, bool) {
	const p = "export"
	if strings.HasPrefix(s, p) && len(s) > len(p) && (s[len(p)] == ' ' || s[len(p)] == '\t') {
		return strings.TrimLeft(s[len(p):], " \t"), true
	}
	return s, false
}

// parseEnvValue reads the value after the first `=`. A single- or
// double-quoted value is its literal content up to the matching quote; an
// unquoted value is trimmed and loses a trailing ` # comment`.
func parseEnvValue(v string) (string, error) {
	t := strings.TrimLeft(v, " \t")
	switch {
	case t == "":
		return "", nil
	case t[0] == '\'':
		return quotedValue(t, '\'', "single")
	case t[0] == '"':
		return quotedValue(t, '"', "double")
	}
	// Unquoted: a `#` after whitespace begins a comment, as in a shell —
	// including a leading one, so `KEY= # c` is an empty value — while a `#`
	// with no space before it (`pa#ss`, or `KEY=#lit` right after the `=`) is
	// part of the value. Scan the raw value, where the leading space is still
	// present, then trim.
	end := len(v)
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			end = i
			break
		}
	}
	return strings.TrimSpace(v[:end]), nil
}

// quotedValue reads a quoted value (t begins with the quote q): its literal
// content up to the matching quote. Only whitespace or a trailing ` # comment`
// may follow the closing quote, so `"abc"def` is rejected rather than silently
// truncated to `abc`.
func quotedValue(t string, q byte, name string) (string, error) {
	i := strings.IndexByte(t[1:], q)
	if i < 0 {
		return "", fmt.Errorf("unterminated %s quote", name)
	}
	rest := strings.TrimLeft(t[i+2:], " \t\r")
	if rest != "" && rest[0] != '#' {
		return "", fmt.Errorf("unexpected text after closing %s quote", name)
	}
	return t[1 : 1+i], nil
}

// selectEnv is the pairs that loadEnvFiles should set, in the order to set
// them, applying the precedence: a variable already present in the
// environment (lookup) wins, and among the files — processed in the given
// order — the first definition of a variable wins. It does not touch the
// environment, so it is testable without mutating global state.
func selectEnv(lookup target.Getenv, files [][]envPair) []envPair {
	var out []envPair
	chosen := map[string]bool{}
	for _, f := range files {
		for _, p := range f {
			if _, ok := lookup(p.Key); ok {
				continue // a real environment variable wins over every file
			}
			if chosen[p.Key] {
				continue // an earlier file already set it
			}
			chosen[p.Key] = true
			out = append(out, p)
		}
	}
	return out
}

// loadEnvFiles reads each --env-file in order and sets, via setenv, the
// variables they define that are not already in the environment (a real
// environment variable, or an earlier file, wins). It runs before config
// resolution, so it feeds both the ${VAR} expansion in target and run files
// and the ORBIT_* env layer — a dotenv file in place of a manual
// `set -a; . file; set +a`.
func loadEnvFiles(paths []string, lookup target.Getenv, setenv func(string, string) error) error {
	if len(paths) == 0 {
		return nil
	}
	files := make([][]envPair, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("--env-file %s: %w", path, err)
		}
		pairs, err := parseEnvFile(f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("--env-file %s: %w", path, err)
		}
		files = append(files, pairs)
	}
	for _, p := range selectEnv(lookup, files) {
		if err := setenv(p.Key, p.Value); err != nil {
			return fmt.Errorf("--env-file: set %s: %w", p.Key, err)
		}
	}
	return nil
}
