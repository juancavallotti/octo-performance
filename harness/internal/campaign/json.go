package campaign

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/juancavallotti/octo-performance/harness/internal/buildinfo"
	"github.com/juancavallotti/octo-performance/harness/internal/result"
)

// harnessInfo stamps every cell with the instrument that produced it. A result that
// cannot be attributed to a version is not a result, and that applies to the harness
// as much as to the runtime it measures.
func harnessInfo() result.Harness {
	i := buildinfo.Get()
	return result.Harness{Version: i.Version, Commit: i.Commit, Dev: i.Dev}
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("campaign: encoding %s: %w", filepath.Base(path), err)
	}
	return writeFile(path, string(b)+"\n")
}

func jsonMarshalString(s string) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
