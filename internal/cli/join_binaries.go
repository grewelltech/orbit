package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bgrewell/orbit/internal/join"
)

// joinBinaries are the orbit binaries `orbit serve` offers a join, by
// arch: orbit-linux-<arch> in dir (--join-binaries), and this binary for
// its own arch when it is linux amd64 or arm64 and dir has none for it. A
// dir that holds neither is an error, since it was named to serve them.
func joinBinaries(dir string) (map[string]string, error) {
	out := map[string]string{}
	if dir != "" {
		for _, arch := range join.Arches {
			p := filepath.Join(dir, "orbit-linux-"+arch)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				out[arch] = p
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("--join-binaries: %w", err)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("--join-binaries %s: holds neither orbit-linux-amd64 nor orbit-linux-arm64", dir)
		}
	}
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		if _, ok := out[runtime.GOARCH]; !ok {
			if self, err := os.Executable(); err == nil {
				out[runtime.GOARCH] = self
			}
		}
	}
	return out, nil
}
