package metrics

import (
	"sort"
	"time"
)

// MetricType represents the Prometheus/OpenMetrics family type.
type MetricType string

const (
	MetricTypeCounter         MetricType = "counter"
	MetricTypeGauge           MetricType = "gauge"
	MetricTypeHistogram       MetricType = "histogram"
	MetricTypeNativeHistogram MetricType = "native_histogram"
	MetricTypeSummary         MetricType = "summary"
	MetricTypeInfo            MetricType = "info"
	MetricTypeStateset        MetricType = "stateset"
	MetricTypeUnknown         MetricType = "unknown"
)

// NamingMode classifies how a metric's Prometheus name relates to its OTLP instrument name.
// Synthesizers use this to avoid lossy reverse-translation guesses (Invariant #7 & DESIGN §7.2).
type NamingMode string

const (
	NamingPrometheusReceiver  NamingMode = "prometheus_receiver"
	NamingUTF8NoTranslation   NamingMode = "utf8_no_translation"
	NamingExplicitOTLP        NamingMode = "explicit_otlp"
	NamingTranslatedAmbiguous NamingMode = "translated_ambiguous"
)

// UsageOrigin classifies where a metric or label reference originated (Invariant #2).
type UsageOrigin string

const (
	OriginAPIUser          UsageOrigin = "api-user"
	OriginRuleEval         UsageOrigin = "rule-eval"
	OriginExcludedExplorer UsageOrigin = "excluded-explorer"
	OriginGrafanaDashboard UsageOrigin = "grafana-dashboard"
	OriginGrafanaAlert     UsageOrigin = "grafana-alert"
	OriginPromAlert        UsageOrigin = "prom-alert"
	OriginPromRecording    UsageOrigin = "prom-recording"
)

// IsLiveSink returns true for origins that directly indicate an active end-consumer.
// Recording-rule evals and recording-rule definitions are NOT live sinks; they feed the DAG only.
func (o UsageOrigin) IsLiveSink() bool {
	switch o {
	case OriginAPIUser, OriginGrafanaDashboard, OriginGrafanaAlert, OriginPromAlert:
		return true
	default:
		return false
	}
}

// ProtectedInternalLabels are structural labels that must NEVER be recommended for DROP_DIMENSION (Invariant #9).
var ProtectedInternalLabels = map[string]struct{}{
	"__name__": {},
	"le":       {},
	"quantile": {},
}

// SeriesStats holds cardinality and label-collapse statistics for a metric family.
type SeriesStats struct {
	TotalSeries      int                     `json:"totalSeries"`
	BucketSeries     int                     `json:"bucketSeries,omitempty"`
	SumCountSeries   int                     `json:"sumCountSeries,omitempty"`
	MemberSeries     map[string]int          `json:"memberSeries,omitempty"`
	Labels           []string                `json:"labels,omitempty"`
	LabelCardinality map[string]int          `json:"labelCardinality,omitempty"`
	SampleLabelSets  []map[string]string     `json:"sampleLabelSets,omitempty"` // Used by offline fixture collapse evaluator
	CollapseEstimates []LabelCollapseEstimate `json:"collapseEstimates,omitempty"`
}

// LabelCollapseEstimate captures the honest `without`-based series collapse projection.
type LabelCollapseEstimate struct {
	DroppedLabels   []string `json:"droppedLabels"`
	CurrentSeries   int      `json:"currentSeries"`
	CollapsedSeries int      `json:"collapsedSeries"`
	SavingsPercent  float64  `json:"savingsPercent"`
	IsSingleLabel   bool     `json:"isSingleLabel"`
	Query           string   `json:"query,omitempty"`
}

// MetricFamily models a Prometheus/OTel metric at the family level so histogram/summary
// series (_bucket, _sum, _count, _created) are never orphaned (Invariant #7).
type MetricFamily struct {
	Name              string        `json:"name"`
	Type              MetricType    `json:"type"`
	Help              string        `json:"help,omitempty"`
	Members           []string      `json:"members"`
	Stats             SeriesStats   `json:"stats"`
	IsNativeHistogram bool          `json:"isNativeHistogram,omitempty"`
	NamingMode        NamingMode    `json:"namingMode,omitempty"`
	OTLPName          string        `json:"otlpName,omitempty"`
	FirstSeenAt       time.Time     `json:"firstSeenAt,omitempty"`
	LastSeenAt        time.Time     `json:"lastSeenAt,omitempty"`
	LastQueriedAt     time.Time     `json:"lastQueriedAt,omitempty"`
	ObservedDuration  time.Duration `json:"observedDuration,omitempty"`
}

// HasClassicBuckets returns true if the family is a classic histogram with _bucket series.
func (f *MetricFamily) HasClassicBuckets() bool {
	if f.IsNativeHistogram || f.Type != MetricTypeHistogram {
		return false
	}
	for _, m := range f.Members {
		if m == f.Name+"_bucket" {
			return true
		}
	}
	return f.Stats.BucketSeries > 0
}

// UsageRef represents a single reference to a metric (exact or regex) from any source.
type UsageRef struct {
	MetricName     string      `json:"metricName,omitempty"`
	MetricRegex    string      `json:"metricRegex,omitempty"`
	IsRegex        bool        `json:"isRegex,omitempty"`
	Origin         UsageOrigin `json:"origin"`
	SourceID       string      `json:"sourceID"`
	SourceName     string      `json:"sourceName"`
	SourceTags     []string    `json:"sourceTags,omitempty"`
	SourceFolder   string      `json:"sourceFolder,omitempty"`
	Query          string      `json:"query,omitempty"`
	Timestamp      time.Time   `json:"timestamp,omitempty"`
	ExplicitLabels []string    `json:"explicitLabels,omitempty"`
	PreservesAll   bool        `json:"preservesAll,omitempty"`
	WithoutLabels  []string    `json:"withoutLabels,omitempty"`
	ClientCaller   string      `json:"clientCaller,omitempty"`
	HTTPPath       string      `json:"httpPath,omitempty"`
}

// LabelUsage accurately tracks used labels across multiple queries/panels,
// respecting bare preservation, `without (...)`, `by (...)`, `label_replace`,
// `label_join`, Grafana legends/links/overrides, and alert templates (Invariant #8).
type LabelUsage struct {
	ExplicitLabels map[string]struct{} `json:"explicitLabels,omitempty"`
	PreservesAll   bool                `json:"preservesAll,omitempty"`
	// WithoutSets records each `without (...)` clause observed for this family.
	// A label is used if it is absent from ANY observed `without` set (union of preserved sets).
	WithoutSets [][]string `json:"withoutSets,omitempty"`
	Sources     []string   `json:"sources,omitempty"`
}

func NewLabelUsage() *LabelUsage {
	return &LabelUsage{
		ExplicitLabels: make(map[string]struct{}),
	}
}

func (u *LabelUsage) MarkExplicit(labels ...string) {
	if u.ExplicitLabels == nil {
		u.ExplicitLabels = make(map[string]struct{})
	}
	for _, l := range labels {
		if l != "" && l != "__name__" {
			u.ExplicitLabels[l] = struct{}{}
		}
	}
}

func (u *LabelUsage) MarkPreservesAll() {
	u.PreservesAll = true
}

func (u *LabelUsage) MarkWithout(excluded []string) {
	cp := make([]string, len(excluded))
	copy(cp, excluded)
	u.WithoutSets = append(u.WithoutSets, cp)
}

func (u *LabelUsage) Merge(other *LabelUsage) {
	if other == nil {
		return
	}
	if other.PreservesAll {
		u.PreservesAll = true
	}
	for l := range other.ExplicitLabels {
		u.MarkExplicit(l)
	}
	for _, ws := range other.WithoutSets {
		u.MarkWithout(ws)
	}
}

// IsLabelUsed returns true if label is structurally protected or consumed by any reference.
func (u *LabelUsage) IsLabelUsed(label string) bool {
	if _, protected := ProtectedInternalLabels[label]; protected {
		return true
	}
	if u == nil {
		return false
	}
	if u.PreservesAll {
		return true
	}
	if _, ok := u.ExplicitLabels[label]; ok {
		return true
	}
	for _, ws := range u.WithoutSets {
		inExcluded := false
		for _, ex := range ws {
			if ex == label {
				inExcluded = true
				break
			}
		}
		if !inExcluded {
			return true
		}
	}
	return false
}

// UnusedLabels returns sorted labels from familyLabels that are NOT used and NOT protected.
func (u *LabelUsage) UnusedLabels(familyLabels []string) []string {
	var unused []string
	seen := make(map[string]struct{})
	for _, l := range familyLabels {
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		if !u.IsLabelUsed(l) {
			unused = append(unused, l)
		}
	}
	sort.Strings(unused)
	return unused
}

// FamilyUsage aggregates all usage evidence for one MetricFamily.
type FamilyUsage struct {
	FamilyName           string      `json:"familyName"`
	LiveRefs             []UsageRef  `json:"liveRefs,omitempty"`
	RuleEvalRefs         []UsageRef  `json:"ruleEvalRefs,omitempty"`
	TransitivelyLive     bool        `json:"transitivelyLive"`
	TransitiveLiveVia    []string    `json:"transitiveLiveVia,omitempty"`
	DeadRecordingChain   []string    `json:"deadRecordingChain,omitempty"`
	UsedBuckets          bool        `json:"usedBuckets"`
	UsedSumOrCount       bool        `json:"usedSumOrCount"`
	UsedBaseMetric       bool        `json:"usedBaseMetric"`
	Labels               *LabelUsage `json:"labels"`
	LastLiveQueryAt      time.Time   `json:"lastLiveQueryAt,omitempty"`
	APIUserQueryCount    int         `json:"apiUserQueryCount"`
	RuleEvalQueryCount   int         `json:"ruleEvalQueryCount"`
}

func (fu *FamilyUsage) HasDirectLiveUsage() bool {
	return len(fu.LiveRefs) > 0
}

func (fu *FamilyUsage) IsLive() bool {
	return fu.HasDirectLiveUsage() || fu.TransitivelyLive
}

// UsageIndex maps metric family names to their aggregated usage.
type UsageIndex struct {
	Families map[string]*FamilyUsage `json:"families"`
}

func NewUsageIndex() *UsageIndex {
	return &UsageIndex{
		Families: make(map[string]*FamilyUsage),
	}
}

func (idx *UsageIndex) GetOrCreate(family string) *FamilyUsage {
	if idx.Families == nil {
		idx.Families = make(map[string]*FamilyUsage)
	}
	fu, ok := idx.Families[family]
	if !ok {
		fu = &FamilyUsage{
			FamilyName: family,
			Labels:     NewLabelUsage(),
		}
		idx.Families[family] = fu
	}
	return fu
}

// WildcardSource flags a single source matching >wildcardThresholdPercent of inventory (Invariant #6).
type WildcardSource struct {
	SourceID        string      `json:"sourceID"`
	SourceName      string      `json:"sourceName"`
	Origin          UsageOrigin `json:"origin"`
	MatchedFamilies int         `json:"matchedFamilies"`
	TotalFamilies   int         `json:"totalFamilies"`
	MatchPercent    float64     `json:"matchPercent"`
	Excluded        bool        `json:"excluded"`
}

// ActionType represents a Trimtab audit decision for a metric family.
type ActionType string

const (
	ActionDropFamily            ActionType = "DROP_FAMILY"
	ActionDropHistogramBuckets  ActionType = "DROP_HISTOGRAM_BUCKETS"
	ActionDropDimensionAdvisory ActionType = "DROP_DIMENSION"
	ActionKeep                  ActionType = "KEEP"
)

// Confidence indicates evidence strength. MEDIUM never triggers a DROP (Invariant #1).
type Confidence string

const (
	ConfidenceHigh   Confidence = "HIGH"
	ConfidenceMedium Confidence = "MEDIUM"
	ConfidenceLow    Confidence = "LOW"
)

// Recommendation describes a proposed or synthesized action for a single metric family.
type Recommendation struct {
	FamilyName      string     `json:"familyName"`
	MetricType      MetricType `json:"metricType"`
	Action          ActionType `json:"action"`
	Confidence      Confidence `json:"confidence"`
	Series          int        `json:"series"`
	Reason          string     `json:"reason"`
	ObservedDays    float64    `json:"observedDays"`
	TrancheNumber   int        `json:"trancheNumber,omitempty"`
	Synthesized     bool       `json:"synthesized"`
	AdvisoryOnly    bool       `json:"advisoryOnly,omitempty"`
	AdvisoryNote    string     `json:"advisoryNote,omitempty"`
	Members         []string   `json:"members,omitempty"`
	NamingMode      NamingMode `json:"namingMode,omitempty"`
	OTLPName        string     `json:"otlpName,omitempty"`
}

// DeadRuleAdvisory advises deletion of recording rules in a dead chain (Invariant #8).
type DeadRuleAdvisory struct {
	RuleName        string   `json:"ruleName"`
	File            string   `json:"file"`
	Group           string   `json:"group"`
	Expr            string   `json:"expr,omitempty"`
	SourceFamilies  []string `json:"sourceFamilies,omitempty"`
	HasOrVectorZero bool     `json:"hasOrVectorZero,omitempty"`
	Note            string   `json:"note,omitempty"`
}

// LabelCollapseAdvisory reports high-cardinality unused labels (read-only in v1, Invariant #9).
type LabelCollapseAdvisory struct {
	FamilyName           string                  `json:"familyName"`
	TargetMetric         string                  `json:"targetMetric"`
	UnusedLabels         []string                `json:"unusedLabels"`
	CombinedEstimate     *LabelCollapseEstimate  `json:"combinedEstimate,omitempty"`
	SingleLabelEstimates []LabelCollapseEstimate `json:"singleLabelEstimates,omitempty"`
}

// RegretEvent records a post-drop api-user query hitting an actively dropped family (Invariant #10).
type RegretEvent struct {
	FamilyName     string    `json:"familyName"`
	DetectedAt     time.Time `json:"detectedAt"`
	Query          string    `json:"query"`
	QueryTimestamp time.Time `json:"queryTimestamp"`
	HitCount       int       `json:"hitCount"`
	DroppedAt      time.Time `json:"droppedAt"`
	Tranche        int       `json:"tranche"`
	ActionTaken    string    `json:"actionTaken"`
}

// Tranche represents one bounded rollout batch (<= maxDropPercentPerTranche).
type Tranche struct {
	Number        int              `json:"number"`
	Series        int              `json:"series"`
	PercentEstate float64          `json:"percentEstate"`
	Items         []Recommendation `json:"items"`
}

// SafetyGateStatus captures every safety invariant check performed during analysis.
type SafetyGateStatus struct {
	ReportOnly            bool          `json:"reportOnly"`
	Blocked               bool          `json:"blocked"`
	BlockReasons          []string      `json:"blockReasons,omitempty"`
	Warnings              []string      `json:"warnings,omitempty"`
	QueryLogConfigured    bool          `json:"queryLogConfigured"`
	QueryLogCoveredDays   float64       `json:"queryLogCoveredDays"`
	RequiredWindowDays    float64       `json:"requiredWindowDays"`
	APIUserQueries        int           `json:"apiUserQueries"`
	RuleEvalQueries       int           `json:"ruleEvalQueries"`
	ExcludedQueries       int           `json:"excludedQueries"`
	BaselineDriftDetected bool          `json:"baselineDriftDetected"`
	BaselineDriftDetails  []string      `json:"baselineDriftDetails,omitempty"`
	SanityCeilingTripped  bool          `json:"sanityCeilingTripped"`
	SoakBlocked           bool          `json:"soakBlocked"`
	SoakRemaining         time.Duration `json:"soakRemaining,omitempty"`
	UnresolvedRegrets     int           `json:"unresolvedRegrets"`
}

// AuditSummary holds top-level numbers for the CLI header and JSON output.
type AuditSummary struct {
	TotalFamilies            int        `json:"totalFamilies"`
	TotalActiveSeries        int        `json:"totalActiveSeries"`
	DroppableFamilies        int        `json:"droppableFamilies"`
	DroppableSeries          int        `json:"droppableSeries"`
	DroppablePercent         float64    `json:"droppablePercent"`
	Confidence               Confidence `json:"confidence"`
	ActiveTranche            int        `json:"activeTranche"`
	TotalTranches            int        `json:"totalTranches"`
	SynthesizedFamilies      int        `json:"synthesizedFamilies"`
	SynthesizedSeries        int        `json:"synthesizedSeries"`
	SynthesizedPercent       float64    `json:"synthesizedPercent"`
	MaxDropPercentPerTranche float64    `json:"maxDropPercentPerTranche"`
	ProtectedFamilies        int        `json:"protectedFamilies"`
	ActiveDropWatchCount     int        `json:"activeDropWatchCount"`
}

// Capabilities declares what actions a synthesizer can safely express (Invariant #7).
type Capabilities struct {
	CanDropFamily               bool         `json:"canDropFamily"`
	CanDropHistogramBucketsOnly bool         `json:"canDropHistogramBucketsOnly"`
	SupportedNamingModes        []NamingMode `json:"supportedNamingModes,omitempty"`
}

func (c Capabilities) SupportsNamingMode(m NamingMode) bool {
	if len(c.SupportedNamingModes) == 0 {
		return true
	}
	for _, sm := range c.SupportedNamingModes {
		if sm == m {
			return true
		}
	}
	return false
}

// AuditReport is the full deterministic output of an analysis run.
type AuditReport struct {
	Name                   string                  `json:"name"`
	GeneratedAt            time.Time               `json:"generatedAt"`
	WindowDays             float64                 `json:"windowDays"`
	SoakWindow             time.Duration           `json:"soakWindow"`
	Target                 string                  `json:"target"`
	Summary                AuditSummary            `json:"summary"`
	Safety                 SafetyGateStatus        `json:"safety"`
	WildcardSources        []WildcardSource        `json:"wildcardSources,omitempty"`
	Tranches               []Tranche               `json:"tranches,omitempty"`
	ActiveDrops            []Recommendation        `json:"activeDrops,omitempty"`
	ProjectedDrops         []Recommendation        `json:"projectedDrops,omitempty"`
	ProtectedReasons       map[string]string       `json:"protectedReasons,omitempty"`
	OversizedFamilies      []Recommendation        `json:"oversizedFamilies,omitempty"`
	CapabilityAdvisories   []Recommendation        `json:"capabilityAdvisories,omitempty"`
	DeadRecordingRules     []DeadRuleAdvisory      `json:"deadRecordingRules,omitempty"`
	LabelCollapseAdvisories []LabelCollapseAdvisory `json:"labelCollapseAdvisories,omitempty"`
	Regrets                []RegretEvent           `json:"regrets,omitempty"`
	SynthesizedOutputPath  string                  `json:"synthesizedOutputPath,omitempty"`
	SynthesizerWarnings    []string                `json:"synthesizerWarnings,omitempty"`
}
