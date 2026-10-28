package join

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shellBuiltins are the POSIX sh builtins and reserved words a join script
// may use as a command word.
var shellBuiltins = []string{"if", "then", "else", "elif", "fi", "for", "in", "do", "done", "while", "until", "case",
	"esac", "return", "exit", "set", "shift", "eval", "export", "umask", "trap", "read", "printf", "echo", "test",
	"[", ":", ".", "true", "false", "break", "continue", "command", "unset", "wait", "cd", "{", "}", "!"}

// unquoted is script with every quoted string's contents, comments and
// here-documents blanked, so only the shell's own words are left: a
// character-level reading of sh's quoting (single, double, backslash) that
// carries its state across lines, as sh does.
func unquoted(script string) string {
	var b strings.Builder
	const (
		none = iota
		single
		double
	)
	state, comment := none, false
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// A here-document's body is data: skip to its delimiter.
		if state == none {
			if m := regexp.MustCompile(`<<-?\s*'?([A-Z]+)'?`).FindStringSubmatch(line); m != nil {
				b.WriteString(line + "\n")
				for i++; i < len(lines) && strings.TrimSpace(lines[i]) != m[1]; i++ {
					b.WriteString("\n")
				}
				b.WriteString("\n")
				continue
			}
		}
		comment = false
		for j := 0; j < len(line); j++ {
			c := line[j]
			switch {
			case comment:
				continue
			case state == single:
				if c == '\'' {
					state = none
					b.WriteByte(c)
				}
				continue
			case state == double:
				if c == '\\' && j+1 < len(line) {
					j++
					continue
				}
				if c == '"' {
					state = none
					b.WriteByte(c)
				}
				continue
			case c == '\\' && j+1 < len(line):
				j++
				b.WriteString("  ")
				continue
			case c == '\'':
				state = single
			case c == '"':
				state = double
			case c == '#' && (j == 0 || line[j-1] == ' ' || line[j-1] == '\t'):
				comment = true
				continue
			}
			b.WriteByte(c)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

var (
	funcDefRE  = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{`)
	assignRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	commandRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	segmentsRE = regexp.MustCompile(`\|\||&&|[|;()` + "`" + `]|\$\(`)
)

// commandWords is every command word script runs: the first word of each
// simple command, past assignments and reserved words; words that are a
// variable ($IPT), a quoted string or a path are not command names here.
func commandWords(script string) (words []string, funcs map[string]bool) {
	funcs = map[string]bool{}
	for _, m := range funcDefRE.FindAllStringSubmatch(script, -1) {
		funcs[m[1]] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(unquoted(script), "\n") {
		// A case pattern (`x86_64 | amd64)`) is not a command: drop it.
		if k := strings.Index(line, ")"); k >= 0 && !strings.Contains(line[:k], "(") {
			line = line[k+1:]
		}
		for _, seg := range segmentsRE.Split(line, -1) {
			for _, w := range strings.Fields(seg) {
				if assignRE.MatchString(w) || w == "then" || w == "do" || w == "else" || w == "!" || w == "{" || w == "}" ||
					w == "if" || w == "elif" || w == "while" || w == "until" {
					continue
				}
				if commandRE.MatchString(w) && !seen[w] {
					seen[w] = true
					words = append(words, w)
				}
				break
			}
		}
	}
	sort.Strings(words)
	return words, funcs
}

// disallowed is the command words of script that are neither stubbed, a
// linked real tool, a shell builtin nor a function the script defines: a
// command the fake host would not see, so a change it could make unseen.
func disallowed(script string) []string {
	allowed := map[string]bool{}
	for _, l := range [][]string{stubbed, realTools, shellBuiltins} {
		for _, w := range l {
			allowed[w] = true
		}
	}
	words, funcs := commandWords(script)
	var out []string
	for _, w := range words {
		if !allowed[w] && !funcs[w] {
			out = append(out, w)
		}
	}
	return out
}

// Every command the script and its helper run is one the fake host stubs
// or links (or a builtin, or the script's own function), so the
// default-deny host sees every call; and the check finds a command it
// would not, such as a sysctl hidden behind `|| true`.
func TestScriptCommandWords(t *testing.T) {
	b, err := Script(testData())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if bad := disallowed(s); len(bad) != 0 {
		t.Errorf("the script runs commands the fake host does not stub or link: %v", bad)
	}
	// The check sees what it must: the script's known commands among them.
	words, _ := commandWords(s)
	for _, w := range []string{"curl", "sha256sum", "ip", "nft", "systemctl", "getent", "awk", "mktemp"} {
		if !contains(words, w) {
			t.Errorf("commandWords missed %q; found %v", w, words)
		}
	}
	for _, w := range words {
		if w == "x86_64" || w == "amd64" || w == "inet" || w == "yes" {
			t.Errorf("commandWords took a case pattern for a command: %q", w)
		}
	}
	// The proof: a sysctl before the prompt, `|| true` so it fails quietly
	// on the fake host, is caught here.
	injected := strings.Replace(s, "\techo \"Plan:\"\n", "\tsysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true\n\techo \"Plan:\"\n", 1)
	if injected == s {
		t.Fatal("no place to inject")
	}
	if bad := disallowed(injected); !contains(bad, "sysctl") {
		t.Errorf("an injected sysctl went unseen: %v", bad)
	}
	// The helper the unit runs, for each firewall setup.
	for _, row := range planRows() {
		if row.name != "nft-native" && row.name != "iptables-legacy" && row.name != "v6" {
			continue
		}
		r := runHost(t, "dash", row.host(t, "dash"), row.args...)
		hb, err := os.ReadFile(filepath.Join(r.root, "usr/local/lib/orbit/join-net.sh"))
		if err != nil {
			t.Fatalf("%s: no helper: %v", row.name, err)
		}
		if bad := disallowed(string(hb)); len(bad) != 0 {
			t.Errorf("%s: the helper runs commands the fake host does not stub or link: %v", row.name, bad)
		}
	}
}
