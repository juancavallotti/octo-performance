package subject

import (
	"context"
	"fmt"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/promx"
)

// Identity is what the running process says it is, as distinct from what the harness
// meant to start.
//
// The distinction is the whole point. The artifact on disk is one fact; the process
// answering on the port is another, and the old lab had no way to tell them apart. A
// stale runtime left over from an earlier cell answers quickly, records excellent
// throughput, and leaves no trace of the substitution in any file the run produces.
type Identity struct {
	// Version is from octo_build_info on the running process, empty when the arm
	// cannot serve metrics.
	Version   string    `json:"version,omitempty"`
	BuildDate time.Time `json:"buildDate,omitzero"`
	Module    string    `json:"servicesModule,omitempty"`

	// Intended is the version the harness meant to start, from the artifact's own
	// `octo version`. It is always present.
	Intended string `json:"intended"`

	// Source says how identity was established: "metrics" when the process was
	// asked, "artifact" when only the file on disk could be. An arm with no admin
	// port can only ever be "artifact", which is why the port-free assertion before
	// start matters more for those arms than for any other.
	Source string `json:"source"`
}

// Agrees reports whether the running process is the artifact the harness intended.
// An identity established only from the artifact cannot disagree, and says so.
func (i Identity) Agrees() bool {
	if i.Version == "" {
		return true
	}
	return i.Version == i.Intended
}

// Identify asks the running process what it is.
//
// It never fails a cell on its own: an arm without an admin port has no way to answer,
// and refusing those would make the 0.4.x-versus-0.5.x comparison — the reason this
// lab exists — impossible to run. The gate decides what a disagreement means.
func (h *Handle) Identify(ctx context.Context) (Identity, error) {
	id := Identity{Intended: h.Caps.Version, Source: "artifact"}

	if !h.Caps.Metrics || h.Endpoints.Admin == "" {
		return id, nil
	}

	body, err := h.Scrape(ctx)
	if err != nil {
		return id, fmt.Errorf("subject: establishing identity: %w", err)
	}
	exp, err := promx.ParseBytes([]byte(body))
	if err != nil {
		return id, fmt.Errorf("subject: parsing exposition for identity: %w", err)
	}

	labels := exp.LabelsOf("octo_build_info")
	if labels == nil {
		return id, fmt.Errorf("subject: %s serves metrics but publishes no octo_build_info", h.Caps.Version)
	}

	id.Source = "metrics"
	id.Version = labels["version"]
	id.Module = labels["services_module"]
	if bd := labels["build_date"]; bd != "" {
		if t, err := time.Parse(time.RFC3339, bd); err == nil {
			id.BuildDate = t
		}
	}
	return id, nil
}
