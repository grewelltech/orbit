package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/target"
)

// fakeEnv is a target.Getenv over a map, so the merge layer is tested
// without reading or mutating the real process environment.
func fakeEnv(m map[string]string) target.Getenv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// parseEnvFile reads KEY=VALUE pairs with the dotenv rules: comments, blank
// lines, export, quotes and `=` or `#` inside values.
func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []envPair
	}{
		{name: "empty", input: "", want: nil},
		{
			name: "comments and blanks are skipped",
			input: "# a comment\n" +
				"   # indented comment\n" +
				"\n" +
				"   \n" +
				"FOO=bar\n",
			want: []envPair{{"FOO", "bar"}},
		},
		{
			name:  "export prefix is stripped",
			input: "export FOO=bar\nexport\tBAZ=qux\n",
			want:  []envPair{{"FOO", "bar"}, {"BAZ", "qux"}},
		},
		{
			name:  "export without a space is part of the name",
			input: "exportFOO=bar\n",
			want:  []envPair{{"exportFOO", "bar"}},
		},
		{
			name:  "surrounding whitespace is trimmed",
			input: "  FOO =  bar  \n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "equals inside an unquoted value",
			input: "FOO=a=b=c\n",
			want:  []envPair{{"FOO", "a=b=c"}},
		},
		{
			name:  "single quotes keep spaces, equals and hashes literal",
			input: "FOO='a b = c # d'\n",
			want:  []envPair{{"FOO", "a b = c # d"}},
		},
		{
			name:  "double quotes keep spaces, equals and hashes literal",
			input: "FOO=\"a b = c # d\"\n",
			want:  []envPair{{"FOO", "a b = c # d"}},
		},
		{
			name:  "inline comment after whitespace is dropped",
			input: "FOO=bar   # trailing note\n",
			want:  []envPair{{"FOO", "bar"}},
		},
		{
			name:  "hash without a preceding space is part of the value",
			input: "FOO=pa#ss\n",
			want:  []envPair{{"FOO", "pa#ss"}},
		},
		{
			name:  "a hash right after = is a literal value",
			input: "FOO=#lit\n",
			want:  []envPair{{"FOO", "#lit"}},
		},
		{
			name:  "a trailing comment after a quoted value is allowed",
			input: "FOO=\"abc\"  # note\n",
			want:  []envPair{{"FOO", "abc"}},
		},
		{
			name:  "an empty value with a trailing comment is empty",
			input: "FOO= # a comment\n",
			want:  []envPair{{"FOO", ""}},
		},
		{
			name:  "empty value",
			input: "FOO=\n",
			want:  []envPair{{"FOO", ""}},
		},
		{
			name:  "value that is only a quoted empty string",
			input: "FOO=''\n",
			want:  []envPair{{"FOO", ""}},
		},
		{
			name:  "CRLF line endings",
			input: "FOO=bar\r\nBAZ=qux\r\n",
			want:  []envPair{{"FOO", "bar"}, {"BAZ", "qux"}},
		},
		{
			name:  "order is preserved and a later duplicate is kept as a pair",
			input: "A=1\nB=2\nA=3\n",
			want:  []envPair{{"A", "1"}, {"B", "2"}, {"A", "3"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvFile(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parseEnvFile: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseEnvFile = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A malformed line is one error naming the line; a bad value is never
// echoed (it may be a secret).
func TestParseEnvFileErrors(t *testing.T) {
	tests := []struct {
		name, input, wantErr string
	}{
		{name: "no equals", input: "FOO bar\n", wantErr: "line 1: not KEY=VALUE"},
		{name: "invalid name digit start", input: "1FOO=bar\n", wantErr: `line 1: "1FOO" is not a valid variable name`},
		{name: "invalid name dash", input: "A-B=c\n", wantErr: `line 1: "A-B" is not a valid variable name`},
		{name: "unterminated double quote", input: "FOO=\"abc\n", wantErr: "line 1: unterminated double quote"},
		{name: "unterminated single quote", input: "FOO='abc\n", wantErr: "line 1: unterminated single quote"},
		{name: "text after a closing double quote", input: "FOO=\"abc\"def\n", wantErr: "line 1: unexpected text after closing double quote"},
		{name: "text after a closing single quote", input: "FOO='abc'x\n", wantErr: "line 1: unexpected text after closing single quote"},
		{name: "error reports the right line", input: "A=1\nB=2\n=bad\n", wantErr: `line 3: "" is not a valid variable name`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEnvFile(strings.NewReader(tc.input))
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// selectEnv applies the precedence: the real environment wins over every
// file, and among the files the first definition wins. The environment is
// not touched.
func TestSelectEnv(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		files [][]envPair
		want  []envPair
	}{
		{
			name:  "a real environment variable wins over the file",
			env:   map[string]string{"FOO": "from-env"},
			files: [][]envPair{{{"FOO", "from-file"}, {"BAR", "b"}}},
			want:  []envPair{{"BAR", "b"}},
		},
		{
			name:  "the empty-string environment value still wins (fills gaps only)",
			env:   map[string]string{"FOO": ""},
			files: [][]envPair{{{"FOO", "from-file"}}},
			want:  nil,
		},
		{
			name: "among files the first definition wins, in given order",
			files: [][]envPair{
				{{"FOO", "first"}, {"ONLY1", "a"}},
				{{"FOO", "second"}, {"ONLY2", "b"}},
			},
			want: []envPair{{"FOO", "first"}, {"ONLY1", "a"}, {"ONLY2", "b"}},
		},
		{
			name:  "within one file the first of a duplicate wins",
			files: [][]envPair{{{"A", "1"}, {"A", "2"}}},
			want:  []envPair{{"A", "1"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectEnv(fakeEnv(tc.env), tc.files)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectEnv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// loadEnvFiles reads the files in order and sets, through the injected
// setenv, only the variables the precedence picks — the real environment is
// never mutated by the test.
func TestLoadEnvFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a.env", "FOO=from-a\nSHARED=a\n")
	b := write("b.env", "BAR=from-b\nSHARED=b\n")

	set := map[string]string{}
	err := loadEnvFiles([]string{a, b}, fakeEnv(map[string]string{"FOO": "real"}), func(k, v string) error {
		set[k] = v
		return nil
	})
	if err != nil {
		t.Fatalf("loadEnvFiles: %v", err)
	}
	// FOO is in the real environment (wins), SHARED is in both files (first
	// wins), BAR is only in the second.
	want := map[string]string{"SHARED": "a", "BAR": "from-b"}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("set = %v, want %v", set, want)
	}
}

// A missing --env-file is one error naming the path, not a panic.
func TestLoadEnvFilesMissing(t *testing.T) {
	err := loadEnvFiles([]string{filepath.Join(t.TempDir(), "nope.env")}, fakeEnv(nil), func(string, string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "--env-file") {
		t.Fatalf("err = %v, want a --env-file error", err)
	}
}

// No --env-file leaves the environment untouched.
func TestLoadEnvFilesNone(t *testing.T) {
	called := false
	if err := loadEnvFiles(nil, fakeEnv(nil), func(string, string) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("setenv called with no --env-file")
	}
}

// unsetForTest removes a variable for the duration of the test and restores
// it afterwards, so a --env-file hook that os.Setenv's it does not leak into
// later tests.
func unsetForTest(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	_ = os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// --env-file loads a dotenv file into the environment before a command runs
// (the root's PersistentPreRunE), and the real environment still wins over
// the file. EnableTraverseRunHooks makes the root hook run even under a
// subcommand group that has its own hook.
func TestEnvFileFlagLoadsBeforeRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "testbed.env")
	if err := os.WriteFile(path, []byte("ORBIT_ENVFILE_ONLY=from-file\nORBIT_ENVFILE_REAL=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsetForTest(t, "ORBIT_ENVFILE_ONLY")
	t.Setenv("ORBIT_ENVFILE_REAL", "from-env")

	root := New("test")
	if !cobra.EnableTraverseRunHooks {
		t.Error("EnableTraverseRunHooks must be set so the root --env-file hook runs under a subcommand group")
	}
	if root.PersistentPreRunE == nil {
		t.Fatal("root has no PersistentPreRunE to load --env-file")
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--env-file", path, "version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := os.Getenv("ORBIT_ENVFILE_ONLY"); got != "from-file" {
		t.Errorf("ORBIT_ENVFILE_ONLY = %q, want from-file (loaded from --env-file)", got)
	}
	if got := os.Getenv("ORBIT_ENVFILE_REAL"); got != "from-env" {
		t.Errorf("ORBIT_ENVFILE_REAL = %q, want from-env (the real environment wins over the file)", got)
	}
}
