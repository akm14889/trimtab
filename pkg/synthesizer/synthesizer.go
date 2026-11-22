package synthesizer

import (
	"trimtab.io/trimtab/pkg/metrics"
)

// GeneratedArtifact is the machine-owned drop configuration emitted behind a collector's
// one-time insertion point (DESIGN §7).
type GeneratedArtifact struct {
	Filename          string   `json:"filename"`
	Content           string   `json:"content"`
	Format            string   `json:"format"`
	DroppedFamilies   int      `json:"droppedFamilies"`
	DroppedSeries     int      `json:"droppedSeries"`
	SkippedAdvisories []string `json:"skippedAdvisories,omitempty"`
}

// Synthesizer converts a safety-gated AuditReport into collector-specific drop rules
// respecting declared Capabilities (Invariant #7).
type Synthesizer interface {
	Name() string
	Capabilities() metrics.Capabilities
	Synthesize(report *metrics.AuditReport) (*GeneratedArtifact, error)
}
