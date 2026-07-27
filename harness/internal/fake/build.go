package fake

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// OctoBinary compiles cmd/fakeocto and returns its path, building it at most once per
// test binary.
//
// Tests could exercise the fake in-process, and some do. This exists for the ones that
// must not: the cell procedure starts a subject through exec.Runner, waits for a port,
// signals it and reads its whole-lifetime rusage, and none of that is being tested if
// the subject is a goroutine. The cost is one compile of a small package.
//
// It deliberately does not take a *testing.T, so this package stays importable by
// cmd/fakeocto without dragging the testing package into a built binary.
func OctoBinary() (string, error) {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeocto-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "fakeocto")
		cmd := osexec.Command("go", "build", "-o", out,
			"github.com/juancavallotti/octo-performance/harness/cmd/fakeocto")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("fake: building fakeocto: %w\n%s", err, b)
			return
		}
		buildPath = out
	})
	return buildPath, buildErr
}
