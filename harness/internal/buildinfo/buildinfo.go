// Package buildinfo carries the version stamped into a released binary.
//
// The values are set by the linker at release time. A binary built with plain
// `go build` reports "dev", and that distinction matters: the lab's first rule is that
// a result which cannot be attributed to a version is not a result, and that applies to
// the harness as much as to the runtime it measures.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	// Dev is true for a binary that was not produced by a release build. Results from
	// one are usable for deciding whether a change worked; they are not publishable.
	Dev bool `json:"dev"`
}

// Get returns the running binary's identity, falling back to Go's own build info for
// the commit when the linker did not stamp one.
func Get() Info {
	i := Info{
		Version:   version,
		Commit:    commit,
		BuildDate: date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Dev:       version == "dev",
	}
	if i.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					i.Commit = s.Value
				case "vcs.modified":
					if s.Value == "true" {
						i.Dev = true
					}
				}
			}
		}
	}
	return i
}

// String is the one-line form printed by `version`.
func (i Info) String() string {
	s := i.Version
	if i.Commit != "" {
		short := i.Commit
		if len(short) > 7 {
			short = short[:7]
		}
		s += " (" + short + ")"
	}
	if i.Dev {
		s += " [dev build — not a publishable result]"
	}
	return fmt.Sprintf("%s  %s  %s", s, i.Platform, i.GoVersion)
}
