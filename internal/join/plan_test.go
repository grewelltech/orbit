package join

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A fakeHost is the host a join script runs against in tests: every command
// that inspects or changes the host is a stub that answers from the row and
// logs its call, every file it touches is under a root directory, and the
// script runs unprivileged with only these commands on its PATH.
type fakeHost struct {
	uid     int               // what `id -u` says
	systemd bool              // /run/systemd/system exists
	tty     string            // the line /dev/tty answers; "" = no terminal
	ports   string            // ip_local_port_range; "" = 32768<TAB>60999
	answers map[string]string // "cmd args" → stdout (exit 0); "!" → exit 1
	files   map[string]string // root-relative path → content, before the run
	absent  []string          // stubbed commands this host does not have
	failOn  string            // a mutating call that fails
	spec    *Spec             // the token's spec; nil = testSpec
	server  *scriptServer     // a real server for real curl; nil = a stub curl
	drop    []string          // real tools this host lacks
	// unvalidated renders the spec without Validate, to show the script
	// refuses on its own what validation stops.
	unvalidated bool
	// extraStep is the render's test-only extra command (see ScriptData).
	extraStep string
	// probe is the server's answer to the script's heartbeat probe: an HTTP
	// code, "removed" (401 with the removed header), or "" (no answer).
	probe string
	// curlVersion is what `curl --version` says; "" = curl 7.88.1.
	curlVersion string
}

// scriptServer is a real TLS server the script downloads from with the real
// curl: its URL, its key's pin, and the SHA-256 the script expects.
type scriptServer struct {
	url, pin, sha string
}

// stubbed are the commands a fakeHost answers for; the rest of a script's
// commands are the real ones.
var stubbed = []string{"id", "uname", "curl", "ip", "getent", "nft", "iptables", "ip6tables", "ufw", "firewall-cmd",
	"useradd", "systemctl", "install"}

// realTools are the plain tools a script uses, linked in as they are.
var realTools = []string{"awk", "sed", "grep", "cut", "tr", "sort", "cat", "cmp", "rm", "mktemp", "chmod", "sha256sum", "cp", "mkdir", "dirname"}

// readOnly are the stubbed calls that only inspect the host, as case
// patterns. Every other call of a stubbed command counts as a change
// (default deny): a test sees any new way the script changes the host.
var readOnly = map[string][]string{
	"ip": {"ip -o -4 route get *", "ip -o -6 route get *", "ip -o addr show", "ip -o addr show *",
		"ip -4 route show exact *", "ip -6 route show exact *"},
	"nft":          {"nft list ruleset", "nft list chain *", "nft -a list chain *"},
	"iptables":     {"iptables -S *", "iptables -C *"},
	"ip6tables":    {"ip6tables -S *", "ip6tables -C *"},
	"getent":       {"getent *"},
	"ufw":          {"ufw status"},
	"firewall-cmd": {"firewall-cmd --state"},
	"systemctl":    {"systemctl is-active *", "systemctl is-enabled *", "systemctl cat *", "systemctl --version"},
}

// fakeOrbit is the binary the fake server serves: it runs and does nothing.
// Its bytes (and so the SHA-256 in every golden) are the same on every host.
// It says on stderr that it ran, so a transcript shows every run of it.
var fakeOrbit = []byte("#!/bin/sh\necho \"fake orbit ran: $*\" >&2\n")

// v4Host is an app host with N6 on ens5 (198.51.100.20/24) and its route
// to the server (192.0.2.10) on ens4, nothing set up yet, and no firewall.
func v4Host() fakeHost {
	return fakeHost{
		systemd: true,
		answers: map[string]string{
			"ip -o -4 route get 198.51.100.1":       "198.51.100.1 dev ens5 src 198.51.100.20 uid 0 \\    cache ",
			"ip -o addr show dev ens5 scope global": "3: ens5    inet 198.51.100.20/24 brd 198.51.100.255 scope global ens5\\       valid_lft forever preferred_lft forever",
			"ip -o addr show": "1: lo    inet 127.0.0.1/8 scope host lo\n" +
				"2: ens4    inet 192.0.2.20/24 brd 192.0.2.255 scope global ens4\n" +
				"3: ens5    inet 198.51.100.20/24 brd 198.51.100.255 scope global ens5\n" +
				"3: ens5    inet 198.51.100.21/24 brd 198.51.100.255 scope global secondary ens5",
			"ip -o -4 route get 192.0.2.10": "192.0.2.10 dev ens4 src 192.0.2.20 uid 0 \\    cache ",
			"nft list ruleset":              "",
			"iptables -S INPUT":             "-P INPUT ACCEPT",
			"ip6tables -S INPUT":            "-P INPUT ACCEPT",
			"systemctl --version":           "systemd 252 (252.22-1~deb12u1)\n+PAM +AUDIT",
		},
	}
}

// with returns h with more answers.
func (h fakeHost) with(kv ...string) fakeHost {
	a := map[string]string{}
	for k, v := range h.answers {
		a[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		a[kv[i]] = kv[i+1]
	}
	h.answers = a
	return h
}

// nftDrop is a native nftables ruleset whose input chain drops.
const nftDrop = "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n" +
	"\t\tct state established,related accept\n\t\tiifname \"lo\" accept\n\t}\n}"

// applied is v4Host after a --yes run with a native nft drop chain: the
// binary, route, rules, user, helper and unit all in place.
func applied(t *testing.T, sh string) fakeHost {
	t.Helper()
	h := v4Host().with(
		"nft list ruleset", nftDrop,
		"ip -4 route show exact 10.250.0.0/16", "10.250.0.0/16 via 198.51.100.1 dev ens5 ",
		"nft -a list chain inet filter input", "table inet filter {\n\tchain input { # handle 1\n"+
			"\t\tiifname \"ens4\" ip saddr 192.0.2.10 tcp dport 9551 accept comment \"orbit-join control ens4 192.0.2.10/32 9551\" # handle 7\n"+
			"\t\tiifname \"ens5\" ip saddr 10.250.0.0/16 meta l4proto { tcp, udp } th dport 32768-60999 accept comment \"orbit-join data ens5 10.250.0.0/16 32768-60999\" # handle 8\n"+
			"\t\tiifname \"ens5\" ip saddr 198.51.100.0/24 meta l4proto { tcp, udp } th dport 32768-60999 accept comment \"orbit-join data ens5 198.51.100.0/24 32768-60999\" # handle 9\n"+
			"\t}\n}",
		"getent passwd orbit", "orbit:x:998:998::/nonexistent:/usr/sbin/nologin",
		"systemctl is-enabled orbit-responder", "enabled",
		"systemctl is-active orbit-responder", "active",
	)
	// The files the first run installed, read back from its own root.
	first := runHost(t, sh, v4Host().with("nft list ruleset", nftDrop,
		"nft -a list chain inet filter input", "table inet filter {\n}"), "--yes")
	h.files = map[string]string{}
	for _, f := range []string{"usr/local/lib/orbit/orbit", "usr/local/lib/orbit/join-net.sh", "etc/systemd/system/orbit-responder.service"} {
		b, err := os.ReadFile(filepath.Join(first.root, f))
		if err != nil {
			t.Fatalf("the first run did not install %s: %v\n%s", f, err, first.out)
		}
		h.files[f] = string(b)
	}
	// The fake orbit registers nothing: the config the real one writes.
	h.files["etc/orbit/responder.yaml"] = "server: https://192.0.2.10:8412\ncredential: " + testCredential + "\n"
	h.probe = "200"
	return h
}

// A hostRun is what a script run against a fakeHost did.
type hostRun struct {
	out   string   // stdout and stderr, the root path removed
	calls []string // the mutating calls, in order
	err   error
	root  string
	dir   string // the run's working directory
	// rootChanged: a file under the root was added, removed or changed.
	rootChanged bool
	// log is every stubbed call, as invoked.
	log string
}

// snapshot is every file and directory under root, with its mode and a
// hash of its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			v += " " + hex.EncodeToString(sum[:])
		}
		m[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runHost renders the script for the fake host and runs it under sh (a
// path, or "busybox sh" style) with args.
func runHost(t *testing.T, sh string, h fakeHost, args ...string) hostRun {
	t.Helper()
	shell := strings.Fields(sh)
	dir := t.TempDir()
	root, bin, ans, tmp := filepath.Join(dir, "root"), filepath.Join(dir, "bin"), filepath.Join(dir, "ans"), filepath.Join(dir, "tmp")
	for _, d := range []string{root, bin, ans, tmp, filepath.Join(root, "proc/sys/net/ipv4")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "log")
	realSh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	orbit := fakeOrbit
	if err := os.WriteFile(filepath.Join(dir, "orbit"), orbit, 0o755); err != nil {
		t.Fatal(err)
	}
	ports := h.ports
	if ports == "" {
		ports = "32768\t60999"
	}
	files := map[string]string{"proc/sys/net/ipv4/ip_local_port_range": ports + "\n"}
	if h.tty != "" {
		files["dev/tty"] = h.tty + "\n"
	}
	for k, v := range h.files {
		files[k] = v
	}
	for k, v := range files {
		p := filepath.Join(root, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(k, "usr/local/lib/orbit/") {
			mode = 0o755 // installed executables
		}
		if err := os.WriteFile(p, []byte(v), mode); err != nil {
			t.Fatal(err)
		}
	}
	if h.systemd {
		if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tools := realTools
	if h.server != nil {
		tools = append(append([]string{}, tools...), "curl")
	}
	for _, tool := range tools {
		if contains(h.drop, tool) {
			continue
		}
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	keys := make([]string, 0, len(h.answers))
	for k := range h.answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, tool := range stubbed {
		if contains(h.absent, tool) || (tool == "curl" && h.server != nil) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#!%s\nprintf '%%s\\n' %s\" $*\" >>%s\ncase %s\" $*\" in\n", realSh, shquote(tool), shquote(log), shquote(tool))
		if h.failOn != "" && strings.HasPrefix(h.failOn, tool+" ") {
			fmt.Fprintf(&b, "%s) printf 'MUT %%s\\n' %s\" $*\" >>%s; echo %s: failed >&2; exit 1 ;;\n", shquote(h.failOn), shquote(tool), shquote(log), shquote(tool))
		}
		for i, k := range keys {
			if !strings.HasPrefix(k, tool+" ") {
				continue
			}
			if h.answers[k] == "!" {
				fmt.Fprintf(&b, "%s) exit 1 ;;\n", shquote(k))
				continue
			}
			f := filepath.Join(ans, fmt.Sprint(i))
			if err := os.WriteFile(f, []byte(h.answers[k]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s) cat %s; exit 0 ;;\n", shquote(k), shquote(f))
		}
		switch tool {
		case "id":
			fmt.Fprintf(&b, "'id -u') echo %d; exit 0 ;;\n", h.uid)
		case "uname":
			b.WriteString("'uname -s') echo Linux; exit 0 ;;\n'uname -m') echo x86_64; exit 0 ;;\n'uname -n') echo apphost; exit 0 ;;\n")
		case "curl":
			cv := h.curlVersion
			if cv == "" {
				cv = "curl 7.88.1 (x86_64-pc-linux-gnu)"
			}
			fmt.Fprintf(&b, "'curl --version') echo %s; exit 0 ;;\n", shquote(cv))
			// The heartbeat probe: the row's answer, as an HTTP code and headers.
			code, hdr := h.probe, ""
			if code == "removed" {
				code, hdr = "401", "Orbit-Responder-State: removed"
			}
			fmt.Fprintf(&b, "*/orbit.v1.JoinService/Heartbeat) while [ $# -gt 0 ]; do [ \"$1\" = -D ] && printf '%%s\\r\\n' 'HTTP/1.1 %s' %s > \"$2\"; shift; done; [ -n %s ] || { printf 000; exit 7; }; printf %%s %s; exit 0 ;;\n",
				code, shquote(hdr), shquote(code), shquote(code))
			// The download: the fake orbit, to curl's -o path.
			fmt.Fprintf(&b, "*) while [ $# -gt 0 ]; do [ \"$1\" = -o ] && cp %s \"$2\"; shift; done; exit 0 ;;\n", shquote(filepath.Join(dir, "orbit")))
		}
		// A read the row did not answer: nothing there.
		for _, m := range readOnly[tool] {
			fmt.Fprintf(&b, "%s) exit 1 ;;\n", strings.ReplaceAll(shquote(m), "*'", "'*"))
		}
		// Anything else changes the host.
		fmt.Fprintf(&b, "*) printf 'MUT %%s\\n' %s\" $*\" >>%s\n", shquote(tool), shquote(log))
		if tool == "install" {
			// install [-d] [-m MODE] SRC DST: the copy, under the root.
			b.WriteString("\tfor a; do :; done; if [ \"$2\" = -d ] || [ \"$1\" = -d ]; then mkdir -p \"$a\"; else n=$#; eval \"s=\\${$((n-1))}\"; mkdir -p \"$(dirname \"$a\" 2>/dev/null || echo .)\"; cp \"$s\" \"$a\"; fi\n")
		}
		b.WriteString("\texit 0 ;;\nesac\n")
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(orbit)
	d := testData()
	d.Nonce = testNonce
	d.SHA256 = map[string]string{"amd64": hex.EncodeToString(sum[:]), "arm64": hex.EncodeToString(sum[:])}
	d.root, d.path, d.extraStep = root, bin, h.extraStep
	if h.server != nil {
		d.ServerURL, d.Pin = h.server.url, h.server.pin
		d.SHA256 = map[string]string{"amd64": h.server.sha, "arm64": h.server.sha}
	}
	if h.spec != nil {
		d.Spec = *h.spec
		d.ServerURL = "https://" + h.spec.Management
	}
	var script []byte
	if h.unvalidated {
		script, err = render(d)
	} else {
		script, err = Script(d)
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := exec.LookPath(shell[0])
	if err != nil {
		t.Skipf("no %s", shell[0])
	}
	cmd := exec.Command(p, append(append(shell[1:], "-s", "--"), args...)...)
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Env = []string{"TMPDIR=" + tmp}
	cmd.Dir = dir
	before := snapshot(t, root)
	out, err := cmd.CombinedOutput()
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the work directory was left behind: %v", left)
	}
	shown := strings.ReplaceAll(strings.ReplaceAll(string(out), root, ""), dir, "<tmp>")
	r := hostRun{out: workRE.ReplaceAllString(shown, "<work>"), err: err, root: root, dir: dir}
	r.rootChanged = !reflect.DeepEqual(before, snapshot(t, root))
	if b, rerr := os.ReadFile(log); rerr == nil {
		r.log = string(b)
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.HasPrefix(l, "MUT ") {
				l = strings.ReplaceAll(strings.TrimPrefix(l, "MUT "), dir, "<tmp>")
				r.calls = append(r.calls, workRE.ReplaceAllString(l, "<work>"))
			}
		}
	}
	return r
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

// workRE matches the script's mktemp work directory.
var workRE = regexp.MustCompile(`<tmp>/tmp/tmp\.[A-Za-z0-9]+`)

// testCredential is the registered responder's credential in the fixtures.
const testCredential = "orc1_CREDCREDCREDCREDCREDCR"

const testNonce = "00112233445566778899aabbccddeeff"

// shells are the POSIX shells every row runs under, where installed.
var shells = []string{"dash", "busybox sh", "bash --posix"}

// planRow is one run of the script against a fake host, compared with its
// golden transcript: what it printed, its exit status and the calls that
// changed the host.
type planRow struct {
	name string
	host func(t *testing.T, sh string) fakeHost
	args []string
	// wantNoChange: the run must make no mutating call at all.
	wantNoChange bool
	wantFail     bool
}

func hostFn(h fakeHost) func(*testing.T, string) fakeHost {
	return func(*testing.T, string) fakeHost { return h }
}

func planRows() []planRow {
	v4 := v4Host()
	nft := v4.with("nft list ruleset", nftDrop, "nft -a list chain inet filter input", "table inet filter {\n}")
	iptLegacy := v4
	iptLegacy.absent = []string{"nft"}
	iptLegacy = iptLegacy.with("iptables -S INPUT", "-P INPUT DROP\n-A INPUT -i lo -j ACCEPT")
	iptNft := v4.with("nft list ruleset", "table ip filter {\n\tchain INPUT {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}",
		"iptables -S INPUT", "-P INPUT DROP")
	ufw := v4.with("ufw status", "Status: active")
	firewalld := v4.with("firewall-cmd --state", "running")
	noSystemd := v4
	noSystemd.systemd = false
	// v6: N6, the pools and the server over IPv6, with a native nft chain.
	v6 := fakeHost{systemd: true, spec: &Spec{UEPools: []string{"2001:db8:a::/48"}, UPFN6: "2001:db8:6::1",
		N6Subnet: "2001:db8:6::/64", Management: "[2001:db8:c::10]:8412"}, answers: map[string]string{
		"ip -o -6 route get 2001:db8:6::1":      "2001:db8:6::1 from :: dev ens5 proto kernel src 2001:db8:6::20 metric 256 pref medium",
		"ip -o addr show dev ens5 scope global": "3: ens5    inet6 2001:db8:6::20/64 scope global \\       valid_lft forever preferred_lft forever",
		"ip -o -6 route get 2001:db8:c::10":     "2001:db8:c::10 from :: via fe80::1 dev ens4 proto ra src 2001:db8:c::20 metric 1024 pref medium",
		"nft list ruleset":                      nftDrop,
		"nft -a list chain inet filter input":   "table inet filter {\n}",
		"ip6tables -S INPUT":                    "-P INPUT ACCEPT",
		"systemctl --version":                   "systemd 252 (252.22-1~deb12u1)",
	}}
	// A server named by a host name: one IPv4 and one IPv6 address picks
	// the IPv4 one; two IPv4 addresses is a refusal.
	named := &Spec{UEPools: testSpec.UEPools, UPFN6: testSpec.UPFN6, N6Subnet: testSpec.N6Subnet, Management: "core.example:8412"}
	byName := nft.with("getent ahostsv4 core.example", "192.0.2.10      STREAM core.example\n192.0.2.10      DGRAM\n192.0.2.10      RAW",
		"getent ahostsv6 core.example", "2001:db8:c::10  STREAM core.example\n2001:db8:c::10  DGRAM\n2001:db8:c::10  RAW")
	byName.spec = named
	twoV4 := byName.with("getent ahostsv4 core.example", "192.0.2.10      STREAM core.example\n192.0.2.11      STREAM")
	tty := func(h fakeHost, a string) fakeHost { h.tty = a; return h }
	uid := func(h fakeHost, u int) fakeHost { h.uid = u; return h }
	fail := func(h fakeHost, c string) fakeHost { h.failOn = c; return h }
	return []planRow{
		// Each firewall setup, applied with --yes.
		{name: "no-filtering", host: hostFn(v4), args: []string{"--yes"}},
		{name: "nft-native", host: hostFn(nft), args: []string{"--yes"}},
		{name: "iptables-legacy", host: hostFn(iptLegacy), args: []string{"--yes"}},
		{name: "iptables-nft", host: hostFn(iptNft), args: []string{"--yes"}},
		{name: "ufw", host: hostFn(ufw), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "firewalld", host: hostFn(firewalld), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "no-systemd", host: hostFn(noSystemd), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "no-systemd-skipped", host: hostFn(noSystemd), args: []string{"--yes", "--no-systemd"}},
		{name: "v6", host: hostFn(v6), args: []string{"--yes"}},
		{name: "server-by-name", host: hostFn(byName), args: []string{"--yes"}},
		{name: "server-name-two-v4", host: hostFn(twoV4), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		// Filtered however it is written: a jump or goto to a chain that
		// drops, a verdict map, a custom iptables target. Each gets the rules
		// at the head of the one filtering chain.
		{name: "nft-jump", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain guard {\n\t\tip saddr != 10.0.0.0/8 drop\n\t}\n"+
			"\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tjump guard\n\t}\n}")), args: []string{"--yes"}},
		{name: "nft-goto", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain dropall {\n\t\tdrop\n\t}\n"+
			"\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tiifname != \"lo\" goto dropall\n\t}\n}")), args: []string{"--yes"}},
		{name: "nft-vmap", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n"+
			"\t\tct state vmap { invalid : drop, established : accept, related : accept }\n\t}\n}")), args: []string{"--yes"}},
		{name: "iptables-custom-target", host: hostFn(func() fakeHost {
			h := v4.with("iptables -S INPUT", "-P INPUT ACCEPT\n-A INPUT -j LOGDROP")
			h.absent = []string{"nft"}
			return h
		}()), args: []string{"--yes"}},
		// An input chain with no rules and policy accept filters nothing.
		{name: "nft-open-chain", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t}\n}")),
			args: []string{"--yes"}},
		// Output that cannot be read is a refusal with the rules to add.
		{name: "nft-unparsed", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n")),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "iptables-unreadable", host: hostFn(v4.with("iptables -S INPUT", "!")), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		// Values that validation would stop, sent straight into the script:
		// it refuses them itself, and nothing runs.
		{name: "hostile-upf-n6", host: hostFn(func() fakeHost {
			h := nft
			h.unvalidated = true
			h.spec = &Spec{UEPools: testSpec.UEPools, UPFN6: "fe80::1%x;touch${IFS}pwned", N6Subnet: testSpec.N6Subnet, Management: testSpec.Management}
			return h
		}()), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "hostile-mgmt-name", host: hostFn(func() fakeHost {
			h := nft
			h.unvalidated = true
			h.spec = &Spec{UEPools: testSpec.UEPools, UPFN6: testSpec.UPFN6, N6Subnet: testSpec.N6Subnet, Management: "a$(touch pwned):8412"}
			return h
		}()), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "hostile-pool", host: hostFn(func() fakeHost {
			h := nft
			h.unvalidated = true
			h.spec = &Spec{UEPools: []string{"10.250.0.0/16;touch pwned"}, UPFN6: testSpec.UPFN6, N6Subnet: testSpec.N6Subnet, Management: testSpec.Management}
			return h
		}()), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		// An input chain at policy drop with no rules filters everything.
		{name: "nft-empty-drop-chain", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}")),
			args: []string{"--yes"}},
		// A multi-line set inside a rule: the chain is read to its own
		// closing brace (filtered, one rule), and the table after it too.
		{name: "nft-multiline-set", host: hostFn(nft.with("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n"+
			"\t\tip saddr {\n\t\t\t203.0.113.1,\n\t\t\t203.0.113.2\n\t\t} drop\n\t}\n}\ntable ip nat {\n\tchain post {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n\t}\n}")),
			args: []string{"--yes"}},
		// The control and N6 interfaces are one: named once.
		{name: "firewalld-one-interface", host: hostFn(firewalld.with("ip -o -4 route get 192.0.2.10", "192.0.2.10 via 198.51.100.254 dev ens5 src 198.51.100.20 uid 0")),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		// A command only inert() stops: with inert() a no-op it would run.
		{name: "inert-stops-a-command", host: hostFn(func() fakeHost { h := nft; h.extraStep = "true $(touch pwned)"; return h }()),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		// Registration: an existing config is left alone unless --reregister;
		// a pending one is resumed.
		{name: "reregister", host: applied, args: []string{"--yes", "--reregister"}},
		{name: "resume-pending", host: func(t *testing.T, sh string) fakeHost {
			h := applied(t, sh)
			delete(h.files, "etc/orbit/responder.yaml")
			h.files["etc/orbit/responder.yaml.pending"] = "pending\n"
			return h
		}, args: []string{"--yes"}},
		// systemd older than 247: no LoadCredential, the config is orbit's.
		// A registered host asks its server first: known (nothing to
		// change), unknown or removed (register again, fresh), or no answer
		// (left as is). The credential never reaches curl's argv.
		{name: "registered-unknown", host: func(t *testing.T, sh string) fakeHost { h := applied(t, sh); h.probe = "401"; return h }, args: []string{"--yes"}},
		{name: "registered-removed", host: func(t *testing.T, sh string) fakeHost { h := applied(t, sh); h.probe = "removed"; return h }, args: []string{"--yes"}},
		{name: "registered-locked-out", host: func(t *testing.T, sh string) fakeHost { h := applied(t, sh); h.probe = "429"; return h }, args: []string{"--yes"}, wantNoChange: true},
		{name: "registered-unreachable", host: func(t *testing.T, sh string) fakeHost { h := applied(t, sh); h.probe = ""; return h }, args: []string{"--yes"}, wantNoChange: true},
		{name: "registered-old-curl", host: func(t *testing.T, sh string) fakeHost {
			h := applied(t, sh)
			h.probe, h.curlVersion = "401", "curl 7.29.0 (x86_64-redhat-linux-gnu)"
			return h
		}, args: []string{"--yes"}, wantNoChange: true},
		{name: "systemd-245", host: hostFn(nft.with("systemctl --version", "systemd 245 (245.4-4ubuntu3)")), args: []string{"--yes"}},
		// The service does not come up: the hints, and the registration stands.
		{name: "start-fails", host: hostFn(fail(nft, "systemctl restart orbit-responder")), args: []string{"--yes"}, wantFail: true},
		{name: "skips", host: hostFn(nft), args: []string{"--yes", "--no-firewall", "--no-route"}},
		// Idempotency: a second run changes nothing; a partial state plans
		// only what is missing; an earlier join's rule is removed.
		{name: "second-run", host: applied, args: []string{"--yes"}, wantNoChange: true},
		{name: "partial", host: func(t *testing.T, sh string) fakeHost {
			h := applied(t, sh)
			return h.with("nft -a list chain inet filter input", "table inet filter {\n}")
		}, args: []string{"--yes"}},
		// Comments this script did not write, hostile ones among them, are
		// left alone and shown inert; nothing in them runs.
		{name: "hostile-comments", host: func(t *testing.T, sh string) fakeHost {
			h := applied(t, sh)
			chain := h.answers["nft -a list chain inet filter input"]
			chain = strings.Replace(chain, "\t}\n}", ""+
				"\t\tiifname \"ens5\" accept comment \"orbit-join data ens5 10.250.0.0/16 $(touch pwned)\" # handle 20\n"+
				"\t\tiifname \"ens5\" accept comment \"orbit-join data ens5 10.0.0.0/8 1;touch pwned\" # handle 21\n"+
				"\t\tiifname \"ens5\" accept comment \"orbit-join it's `touch pwned`\" # handle 22\n"+
				"\t\tiifname \"ens5\" accept comment \"orbit-join data ens5 10.1.0.0/16 1-2\" # handle 23;touch pwned\n"+
				"\t\tiifname \"ens5\" accept comment \"orbit-join data ens5\n10.2.0.0/16 1-2\" # handle 24\n"+
				"\t}\n}", 1)
			return h.with("nft -a list chain inet filter input", chain)
		}, args: []string{"--yes"}, wantNoChange: true},
		{name: "stale-rule", host: func(t *testing.T, sh string) fakeHost {
			h := applied(t, sh)
			h.ports = "40000\t50000"
			return h
		}, args: []string{"--yes"}},
		// Confirmation: nothing changes without it.
		{name: "no-tty", host: hostFn(nft), wantNoChange: true},
		{name: "tty-no", host: hostFn(tty(nft, "n")), wantNoChange: true},
		{name: "tty-yes", host: hostFn(tty(nft, "y"))},
		{name: "dry-run", host: hostFn(tty(nft, "y")), args: []string{"--dry-run", "--yes"}, wantNoChange: true},
		{name: "dry-run-not-root", host: hostFn(uid(nft, 1000)), args: []string{"--dry-run"}, wantNoChange: true},
		// Refusals.
		{name: "not-root", host: hostFn(uid(nft, 1000)), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "unknown-flag", host: hostFn(nft), args: []string{"--yes", "--force"}, wantNoChange: true, wantFail: true},
		{name: "flag-without-value", host: hostFn(nft), args: []string{"--n6-ip"}, wantNoChange: true, wantFail: true},
		{name: "no-route-to-upf", host: hostFn(nft.with("ip -o -4 route get 198.51.100.1", "!")), args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "upf-via-gateway", host: hostFn(nft.with("ip -o -4 route get 198.51.100.1", "198.51.100.1 via 192.0.2.1 dev ens4 src 192.0.2.20 uid 0")),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "two-n6-addresses", host: hostFn(nft.with("ip -o addr show dev ens5 scope global",
			"3: ens5    inet 198.51.100.20/24 scope global ens5\n3: ens5    inet 198.51.100.21/24 scope global secondary ens5")),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "n6-ip-chosen", host: hostFn(nft.with("ip -o -4 route get 198.51.100.1 from 198.51.100.21", "198.51.100.1 from 198.51.100.21 dev ens5 uid 0")),
			args: []string{"--yes", "--n6-ip", "198.51.100.21"}},
		{name: "n6-ip-not-local", host: hostFn(nft), args: []string{"--yes", "--n6-ip=203.0.113.9"}, wantNoChange: true, wantFail: true},
		{name: "bind-chosen", host: hostFn(nft.with("ip -o -4 route get 192.0.2.10 from 198.51.100.20", "192.0.2.10 from 198.51.100.20 via 198.51.100.254 dev ens5 uid 0")),
			args: []string{"--yes", "--bind", "198.51.100.20:7000"}},
		{name: "several-nft-chains", host: hostFn(nft.with("nft list ruleset", nftDrop+"\ntable ip extra {\n\tchain in {\n\t\ttype filter hook input priority 10; policy accept;\n\t\ttcp dport 22 drop\n\t}\n}")),
			args: []string{"--yes"}, wantNoChange: true, wantFail: true},
		{name: "fails-partway", host: hostFn(fail(nft, "ip -4 route replace 10.250.0.0/16 via 198.51.100.1 dev ens5")), args: []string{"--yes"}, wantFail: true},
	}
}

// The script against each fake host matches its golden transcript (go test
// -update rewrites them), under every POSIX shell on the host; the runs that
// must not change the host make no mutating call.
func TestJoinPlan(t *testing.T) {
	for _, row := range planRows() {
		for _, sh := range shells {
			t.Run(row.name+"/"+strings.Fields(sh)[0], func(t *testing.T) {
				if _, err := exec.LookPath(strings.Fields(sh)[0]); err != nil {
					t.Skipf("no %s", sh)
				}
				if sh == "busybox sh" && busyboxStandalone() {
					t.Skip("this busybox runs its own applets ahead of PATH, past the stubs; it parses the script (TestScriptGolden), and the LXD Alpine run executes it")
				}
				r := runHost(t, sh, row.host(t, sh), row.args...)
				if (r.err != nil) != row.wantFail {
					t.Errorf("err = %v, want failure %v\n%s", r.err, row.wantFail, r.out)
				}
				if row.wantNoChange && (len(r.calls) != 0 || r.rootChanged) {
					t.Errorf("changed the host: %q (files changed: %v)", r.calls, r.rootChanged)
				}
				// Nothing downloaded runs before the plan is confirmed.
				if row.wantNoChange && strings.Contains(r.out, "fake orbit ran") {
					t.Errorf("the downloaded orbit ran without a confirmed plan:\n%s", r.out)
				}
				if _, err := os.Stat(filepath.Join(r.dir, "pwned")); err == nil {
					t.Errorf("a value from the host ran as a command")
				}
				if strings.Contains(r.log, testCredential) {
					t.Errorf("a command line carried the responder credential:\n%s", r.log)
				}
				got := r.out + "--- exit: " + exitOf(r.err) + "\n--- changes:\n" + strings.Join(r.calls, "\n") + "\n"
				golden := filepath.Join("testdata", "plan", row.name+".txt")
				if *update && sh == shells[0] {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Errorf("transcript differs from %s (go test -update to accept):\n%s", golden, got)
				}

				// The same host without --yes and with no terminal: everything
				// before the confirmation, which must change nothing and run
				// nothing downloaded.
				if sh != shells[0] || !contains(row.args, "--yes") {
					return
				}
				h := row.host(t, sh)
				h.tty = ""
				var args []string
				for _, a := range row.args {
					if a != "--yes" {
						args = append(args, a)
					}
				}
				r = runHost(t, sh, h, args...)
				if len(r.calls) != 0 || r.rootChanged || strings.Contains(r.out, "fake orbit ran") {
					t.Errorf("without a confirmation: changes %q, files changed %v\n%s", r.calls, r.rootChanged, r.out)
				}
				if _, err := os.Stat(filepath.Join(r.dir, "pwned")); err == nil {
					t.Errorf("without a confirmation, a value from the host ran as a command")
				}
			})
		}
	}
}

// busyboxStandalone reports whether busybox's sh runs its applets even
// when PATH has none (Debian's busybox-static does).
func busyboxStandalone() bool {
	return exec.Command("busybox", "sh", "-c", "PATH=/nonexistent; id -u").Run() == nil
}

func exitOf(err error) string {
	if err == nil {
		return "0"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprint(ee.ExitCode())
	}
	return err.Error()
}

// The helper the unit runs as root (join-net.sh) is POSIX sh that shellcheck
// accepts, for each firewall setup, and it holds exactly the plan's route
// and rules.
func TestHelperLint(t *testing.T) {
	rows := map[string]bool{"nft-native": true, "iptables-legacy": true, "v6": true, "no-filtering": true, "skips": true}
	for _, row := range planRows() {
		if !rows[row.name] {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			r := runHost(t, "dash", row.host(t, "dash"), row.args...)
			helper := filepath.Join(r.root, "usr/local/lib/orbit/join-net.sh")
			b, err := os.ReadFile(helper)
			if err != nil {
				t.Fatalf("no helper: %v\n%s", err, r.out)
			}
			if !strings.HasPrefix(string(b), "#!/bin/sh\n") || !strings.Contains(string(b), "\nset -eu\nPATH=/usr/sbin:/usr/bin:/sbin:/bin\n") {
				t.Errorf("helper header:\n%s", b)
			}
			for _, shell := range []string{"sh", "dash"} {
				if p, err := exec.LookPath(shell); err == nil {
					if out, err := exec.Command(p, "-n", helper).CombinedOutput(); err != nil {
						t.Errorf("%s -n: %v\n%s", shell, err, out)
					}
				}
			}
			switch sc, err := exec.LookPath("shellcheck"); {
			case err == nil:
				if out, err := exec.Command(sc, "-s", "sh", helper).CombinedOutput(); err != nil {
					t.Errorf("shellcheck: %v\n%s", err, out)
				}
			case os.Getenv("CI") != "":
				t.Error("shellcheck is not installed; CI must run it")
			}
		})
	}
}
