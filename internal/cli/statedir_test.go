package cli

import (
	"path/filepath"
	"testing"
)

func TestResolveStateDir(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		got, err := resolveStateDir("/tmp/somewhere")
		if err != nil || got != "/tmp/somewhere" {
			t.Fatalf("got %q, %v; want the flag verbatim", got, err)
		}
	})

	t.Run("none disables", func(t *testing.T) {
		got, err := resolveStateDir("none")
		if err != nil || got != "" {
			t.Fatalf(`got %q, %v; want "" for "none"`, got, err)
		}
	})

	t.Run("XDG state home", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/xdg")
		got, _ := resolveStateDir("")
		if want := filepath.Join("/xdg", "orbit", "runs"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("home when XDG is unset", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/home/someone")
		got, _ := resolveStateDir("")
		if want := filepath.Join("/home/someone", ".local", "state", "orbit", "runs"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// The regression: systemd starts a service with no environment at all, so
	// both XDG_STATE_HOME and HOME are unset. Returning "" here silently
	// disabled persistence — the feature turned itself off and said nothing.
	t.Run("system default when neither is set", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "")
		got, err := resolveStateDir("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == "" {
			t.Fatal("persistence disabled itself because HOME was unset; only \"none\" may disable it")
		}
		if want := filepath.Join(SystemStateDir, "runs"); got != want {
			t.Errorf("got %q, want the system default %q", got, want)
		}
	})
}
