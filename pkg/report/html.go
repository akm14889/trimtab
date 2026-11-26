package report

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/metrics"
)

type htmlReportData struct {
	Name                string
	Target              string
	WindowLabel         string
	GeneratedAt         string
	Confidence          string
	ConfidenceClass     string
	StatusLabel         string
	StatusClass         string
	StatusNotice        string
	QueryLogConfigured  bool
	CoverageRatio       string
	APIUserQueries      string
	RuleEvalQueries     string
	ExcludedQueries     string
	TotalFamilies       string
	TotalActiveSeries   string
	DroppableFamilies   string
	DroppableSeries     string
	DroppablePercent    string
	DroppableWidth      string
	ProtectedFamilies   string
	ActiveTranche       int
	TotalTranches       int
	SynthesizedFamilies string
	SynthesizedSeries   string
	SynthesizedPercent  string
	MaxDropPerTranche   string
	ActiveDropWatch     string
	WildcardSummary     string
	BlockReasons        []string
	Warnings            []string
	Regrets             []htmlRegretRow
	UnresolvedRegrets   int
	PendingRevertText   string
	DropsHeading        string
	Drops               []htmlDropRow
	Tranches            []htmlTrancheRow
	Wildcards           []htmlWildcardRow
	DeadRules           []htmlDeadRuleRow
	CollapseRows        []htmlCollapseRow
	Oversized           []htmlAdvisoryRow
	CapabilityNotes     []htmlAdvisoryRow
	SynthWarnings       []string
}

type htmlRegretRow struct {
	FamilyName  string
	HitCount    int
	Query       string
	ActionTaken string
}

type htmlDropRow struct {
	FamilyName    string
	MetricType    string
	Action        string
	Series        string
	Reason        string
	TrancheNumber int
}

type htmlTrancheRow struct {
	Number        int
	FamilyCount   int
	Series        string
	PercentEstate string
}

type htmlWildcardRow struct {
	SourceName         string
	Origin             string
	Matched            string
	MatchPercent       string
	Excluded           bool
	RecommendedExclude string
}

type htmlDeadRuleRow struct {
	RuleName        string
	File            string
	Group           string
	HasOrVectorZero bool
	Note            string
}

type htmlCollapseRow struct {
	TargetMetric string
	Labels       string
	Current      string
	Collapsed    string
	Savings      string
	IsZero       bool
}

type htmlAdvisoryRow struct {
	FamilyName string
	Series     string
	Note       string
}

var htmlReportTmpl = template.Must(template.New("trimtab-report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Trimtab Audit Report — {{.Name}}</title>
  <style>
    :root {
      --bg: #f8fafc;
      --surface: #ffffff;
      --border: #e2e8f0;
      --text: #0f172a;
      --muted: #64748b;
      --accent: #2563eb;
      --good: #15803d;
      --good-bg: #dcfce7;
      --warn: #b45309;
      --warn-bg: #fef3c7;
      --bad: #b91c1c;
      --bad-bg: #fee2e2;
      --info-bg: #eff6ff;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 24px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
      line-height: 1.5;
    }
    .container { max-width: 1200px; margin: 0 auto; }
    .header {
      background: #0f172a;
      color: #f8fafc;
      padding: 20px 24px;
      border-radius: 10px;
      display: flex;
      flex-wrap: wrap;
      justify-content: space-between;
      align-items: center;
      gap: 16px;
      margin-bottom: 20px;
    }
    .header h1 { margin: 0 0 4px 0; font-size: 1.35rem; }
    .meta { color: #cbd5e1; font-size: 0.875rem; }
    .badges { display: flex; gap: 8px; flex-wrap: wrap; }
    .badge {
      display: inline-block;
      padding: 4px 10px;
      border-radius: 999px;
      font-size: 0.75rem;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0.03em;
    }
    .badge-good { background: var(--good-bg); color: var(--good); }
    .badge-warn { background: var(--warn-bg); color: var(--warn); }
    .badge-bad { background: var(--bad-bg); color: var(--bad); }
    .badge-info { background: var(--info-bg); color: var(--accent); }
    .banner {
      border-radius: 8px;
      padding: 14px 18px;
      margin-bottom: 16px;
      border: 1px solid var(--border);
    }
    .banner-bad { background: var(--bad-bg); border-color: #fca5a5; color: #7f1d1d; }
    .banner-warn { background: var(--warn-bg); border-color: #fcd34d; color: #78350f; }
    .banner-info { background: var(--info-bg); border-color: #bfdbfe; color: #1e3a8a; }
    .banner h3 { margin: 0 0 6px 0; font-size: 0.95rem; }
    .banner ul { margin: 0; padding-left: 20px; }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 14px;
      margin-bottom: 22px;
    }
    .card {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 16px;
    }
    .card-label { font-size: 0.78rem; text-transform: uppercase; color: var(--muted); font-weight: 600; }
    .card-value { font-size: 1.5rem; font-weight: 700; margin: 6px 0 4px 0; }
    .card-sub { font-size: 0.82rem; color: var(--muted); }
    .bar { height: 6px; background: #e2e8f0; border-radius: 999px; margin-top: 8px; overflow: hidden; }
    .bar-fill { height: 100%; background: var(--accent); }
    .section {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 18px;
      margin-bottom: 18px;
      overflow-x: auto;
    }
    .section h2 { margin: 0 0 12px 0; font-size: 1.05rem; }
    table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
    th, td { text-align: left; padding: 9px 10px; border-bottom: 1px solid var(--border); vertical-align: top; }
    th { font-size: 0.75rem; text-transform: uppercase; color: var(--muted); font-weight: 700; }
    .num { text-align: right; font-variant-numeric: tabular-nums; }
    code {
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 0.82rem;
      background: #f1f5f9;
      padding: 2px 5px;
      border-radius: 4px;
    }
    .empty { color: var(--muted); font-size: 0.9rem; margin: 0; }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <div>
        <h1>Trimtab Audit Report &mdash; {{.Name}}</h1>
        <div class="meta">Scope: single-tenant (per family) &middot; Window: {{.WindowLabel}} &middot; Target: {{.Target}} &middot; Generated: {{.GeneratedAt}}</div>
      </div>
      <div class="badges">
        <span class="badge {{.ConfidenceClass}}">{{.Confidence}} Confidence</span>
        <span class="badge {{.StatusClass}}">{{.StatusLabel}}</span>
      </div>
    </div>

    {{if .Regrets}}
    <div class="banner banner-bad">
      <h3>Regret Check (Forward-Only): {{len .Regrets}} Regret(s) Detected &mdash; Next Tranche Blocked</h3>
      <ul>
        {{range .Regrets}}
        <li><code>{{.FamilyName}}</code> ({{.HitCount}} api-user hit(s), query <code>{{.Query}}</code>) &rarr; {{.ActionTaken}}</li>
        {{end}}
      </ul>
    </div>
    {{end}}

    {{if .BlockReasons}}
    <div class="banner banner-warn">
      <h3>Safety Gate (Synthesis Held &mdash; Pass-Through / No-Op Only)</h3>
      <ul>
        {{range .BlockReasons}}<li>{{.}}</li>{{end}}
      </ul>
    </div>
    {{end}}

    {{if .StatusNotice}}
    <div class="banner banner-info">
      <h3>Rollout Status</h3>
      <div>{{.StatusNotice}}</div>
    </div>
    {{end}}

    {{if .Warnings}}
    <div class="banner banner-warn">
      <h3>Warnings</h3>
      <ul>
        {{range .Warnings}}<li>{{.}}</li>{{end}}
      </ul>
    </div>
    {{end}}

    <div class="grid">
      <div class="card">
        <div class="card-label">Active Series</div>
        <div class="card-value">{{.TotalActiveSeries}}</div>
        <div class="card-sub">{{.TotalFamilies}} metric families</div>
      </div>
      <div class="card">
        <div class="card-label">Droppable Potential</div>
        <div class="card-value">{{.DroppableSeries}} <small>({{.DroppablePercent}}%)</small></div>
        <div class="card-sub">{{.DroppableFamilies}} families (cap {{.MaxDropPerTranche}}%/tranche)</div>
        <div class="bar"><div class="bar-fill" style="width: {{.DroppableWidth}}%;"></div></div>
      </div>
      <div class="card">
        <div class="card-label">Synthesized Now</div>
        <div class="card-value">{{.SynthesizedSeries}} <small>({{.SynthesizedPercent}}%)</small></div>
        <div class="card-sub">Tranche {{.ActiveTranche}}/{{.TotalTranches}} &middot; {{.SynthesizedFamilies}} families</div>
      </div>
      <div class="card">
        <div class="card-label">Query Log Coverage</div>
        {{if .QueryLogConfigured}}
        <div class="card-value">{{.CoverageRatio}}</div>
        <div class="card-sub">{{.APIUserQueries}} api-user &middot; {{.RuleEvalQueries}} rule-eval &middot; {{.ExcludedQueries}} excluded</div>
        {{else}}
        <div class="card-value">None</div>
        <div class="card-sub">Report-Only mode (query log required for drops)</div>
        {{end}}
      </div>
      <div class="card">
        <div class="card-label">Protected (Safe)</div>
        <div class="card-value">{{.ProtectedFamilies}}</div>
        <div class="card-sub">Wildcards: {{.WildcardSummary}}</div>
      </div>
      <div class="card">
        <div class="card-label">Regret Watch</div>
        <div class="card-value">{{.UnresolvedRegrets}} unresolved</div>
        <div class="card-sub">Watching {{.ActiveDropWatch}} active drops{{if .PendingRevertText}} &middot; pending revert: {{.PendingRevertText}}{{end}}</div>
      </div>
    </div>

    <div class="section">
      <h2>{{.DropsHeading}}</h2>
      {{if .Drops}}
      <table>
        <thead>
          <tr><th>Metric Family</th><th>Type</th><th>Action</th><th class="num">Series</th><th>Reason</th></tr>
        </thead>
        <tbody>
          {{range .Drops}}
          <tr>
            <td><code>{{.FamilyName}}</code></td>
            <td>{{.MetricType}}</td>
            <td><code>{{.Action}}</code></td>
            <td class="num">{{.Series}}</td>
            <td>{{.Reason}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      {{else}}
      <p class="empty">No active or projected drops in this audit.</p>
      {{end}}
    </div>

    {{if .Tranches}}
    <div class="section">
      <h2>Planned Tranches</h2>
      <table>
        <thead>
          <tr><th>Tranche</th><th class="num">Families</th><th class="num">Series</th><th class="num">% of Estate</th></tr>
        </thead>
        <tbody>
          {{range .Tranches}}
          <tr>
            <td>Tranche {{.Number}}</td>
            <td class="num">{{.FamilyCount}}</td>
            <td class="num">{{.Series}}</td>
            <td class="num">{{.PercentEstate}}%</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{end}}

    {{if .Wildcards}}
    <div class="section">
      <h2>Wildcard Protectors</h2>
      <table>
        <thead>
          <tr><th>Source</th><th>Origin</th><th class="num">Matched</th><th class="num">%</th><th>Status</th><th>Recommended Exclude</th></tr>
        </thead>
        <tbody>
          {{range .Wildcards}}
          <tr>
            <td>{{.SourceName}}</td>
            <td>{{.Origin}}</td>
            <td class="num">{{.Matched}}</td>
            <td class="num">{{.MatchPercent}}%</td>
            <td>{{if .Excluded}}<span class="badge badge-good">Excluded</span>{{else}}<span class="badge badge-warn">Flagged</span>{{end}}</td>
            <td>{{if .RecommendedExclude}}<code>{{.RecommendedExclude}}</code>{{else}}&mdash;{{end}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{end}}

    {{if .DeadRules}}
    <div class="section">
      <h2>Advisory: Dead Recording Rules to Delete</h2>
      <table>
        <thead>
          <tr><th>Rule Name</th><th>File</th><th>Group</th><th>Note</th></tr>
        </thead>
        <tbody>
          {{range .DeadRules}}
          <tr>
            <td><code>{{.RuleName}}</code></td>
            <td><code>{{.File}}</code></td>
            <td><code>{{.Group}}</code></td>
            <td>{{if .HasOrVectorZero}}<span class="badge badge-warn">or vector(0)</span> {{end}}{{.Note}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{end}}

    {{if .CollapseRows}}
    <div class="section">
      <h2>Advisory: High-Cardinality Unused Labels (Honest <code>without</code> Collapse)</h2>
      <table>
        <thead>
          <tr><th>Metric</th><th>Unused Labels</th><th class="num">Today &rarr; Collapsed</th><th class="num">Savings</th></tr>
        </thead>
        <tbody>
          {{range .CollapseRows}}
          <tr>
            <td><code>{{.TargetMetric}}</code></td>
            <td><code>{{.Labels}}</code></td>
            <td class="num">{{.Current}} &rarr; {{.Collapsed}}</td>
            <td class="num">{{if .IsZero}}<span class="badge badge-warn">{{.Savings}}</span>{{else}}{{.Savings}}{{end}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{end}}

    {{if or .Oversized .CapabilityNotes .SynthWarnings}}
    <div class="section">
      <h2>Capability &amp; Sizing Advisories</h2>
      <table>
        <thead>
          <tr><th>Metric Family</th><th class="num">Series</th><th>Note</th></tr>
        </thead>
        <tbody>
          {{range .Oversized}}
          <tr><td><code>{{.FamilyName}}</code></td><td class="num">{{.Series}}</td><td>{{.Note}}</td></tr>
          {{end}}
          {{range .CapabilityNotes}}
          <tr><td><code>{{.FamilyName}}</code></td><td class="num">{{.Series}}</td><td>{{.Note}}</td></tr>
          {{end}}
          {{range .SynthWarnings}}
          <tr><td>&mdash;</td><td class="num">&mdash;</td><td>{{.}}</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
    {{end}}
  </div>
</body>
</html>
`))

func renderHTML(rep *metrics.AuditReport) (string, error) {
	width := rep.Summary.DroppablePercent
	if width < 0 {
		width = 0
	} else if width > 100 {
		width = 100
	}

	confClass := "badge-warn"
	switch rep.Summary.Confidence {
	case metrics.ConfidenceHigh:
		confClass = "badge-good"
	case metrics.ConfidenceLow:
		confClass = "badge-bad"
	}

	statusLabel := "READY"
	statusClass := "badge-info"
	switch {
	case len(rep.Regrets) > 0 || rep.Safety.UnresolvedRegrets > 0:
		statusLabel = "REGRET BLOCKED"
		statusClass = "badge-bad"
	case rep.Safety.Blocked:
		statusLabel = "BLOCKED"
		statusClass = "badge-bad"
	case rep.Safety.ReportOnly:
		statusLabel = "REPORT-ONLY"
		statusClass = "badge-warn"
	case rep.Summary.SynthesizedFamilies > 0:
		if rep.Safety.ActiveTrancheStatus != "" {
			statusLabel = strings.ToUpper(rep.Safety.ActiveTrancheStatus)
		} else {
			statusLabel = "ACTIVE"
		}
		statusClass = "badge-good"
	}

	statusNotice := ""
	totalTranches := maxInt(rep.Summary.TotalTranches, rep.Summary.ActiveTranche)
	if !rep.Safety.ReportOnly && !rep.Safety.Blocked {
		switch {
		case rep.Safety.PendingConfirmation:
			statusNotice = fmt.Sprintf("Tranche %d/%d is awaiting confirmation (waiting for dropped series to reach 0 in inventory, or pass --confirm-tranche).", rep.Summary.ActiveTranche, totalTranches)
		case rep.Safety.NextTrancheReady && len(rep.Tranches) > 0:
			nextT := rep.Tranches[0]
			statusNotice = fmt.Sprintf("Next eligible tranche (%d/%d) is ready (observation-only; run 'trimtab synth --activate' to activate).", nextT.Number, maxInt(rep.Summary.TotalTranches, nextT.Number))
		case rep.Summary.TotalTranches > rep.Summary.ActiveTranche:
			nextTranche := rep.Summary.ActiveTranche + 1
			if rep.Safety.UnresolvedRegrets > 0 {
				if len(rep.Safety.PendingRevertFamilies) > 0 {
					statusNotice = fmt.Sprintf("Next tranche (%d/%d) BLOCKED until collector revert is applied for %s and --resolve-regret is run.", nextTranche, rep.Summary.TotalTranches, strings.Join(rep.Safety.PendingRevertFamilies, ", "))
				} else {
					statusNotice = fmt.Sprintf("Next tranche (%d/%d) BLOCKED until regrets are reviewed (revert confirmed; run 'trimtab analyze --resolve-regret' to unblock).", nextTranche, rep.Summary.TotalTranches)
				}
			} else if rep.Safety.SoakBlocked {
				statusNotice = fmt.Sprintf("Next tranche (%d/%d) soaking (%s remaining with 0 regrets).", nextTranche, rep.Summary.TotalTranches, formatDurationShort(rep.Safety.SoakRemaining))
			} else {
				statusNotice = fmt.Sprintf("Next tranche (%d/%d) unlocks after %s soak with 0 regrets.", nextTranche, rep.Summary.TotalTranches, formatDurationShort(rep.SoakWindow))
			}
		}
	}

	genAt := "N/A"
	if !rep.GeneratedAt.IsZero() {
		genAt = rep.GeneratedAt.UTC().Format(time.RFC3339)
	}

	dropsHeading := fmt.Sprintf("Tranche %d Drops", rep.Summary.ActiveTranche)
	selectedDrops := rep.ActiveDrops
	if len(selectedDrops) == 0 && len(rep.ProjectedDrops) > 0 {
		selectedDrops = rep.ProjectedDrops
		dropsHeading = "Projected Drops (Report-Only)"
	} else if len(selectedDrops) == 0 && !rep.Safety.ReportOnly && !rep.Safety.Blocked && len(rep.Tranches) > 0 && rep.Safety.NextTrancheReady {
		selectedDrops = rep.Tranches[0].Items
		dropsHeading = fmt.Sprintf("Next Eligible Tranche (%d/%d — Observation-Only)", rep.Tranches[0].Number, rep.Summary.TotalTranches)
	} else if rep.Safety.ActiveTrancheStatus != "" && len(selectedDrops) > 0 {
		dropsHeading = fmt.Sprintf("Tranche %d Drops [%s]", rep.Summary.ActiveTranche, rep.Safety.ActiveTrancheStatus)
	}

	data := htmlReportData{
		Name:                rep.Name,
		Target:              rep.Target,
		WindowLabel:         formatWindowLabel(rep.WindowDays),
		GeneratedAt:         genAt,
		Confidence:          string(rep.Summary.Confidence),
		ConfidenceClass:     confClass,
		StatusLabel:         statusLabel,
		StatusClass:         statusClass,
		StatusNotice:        statusNotice,
		QueryLogConfigured:  rep.Safety.QueryLogConfigured,
		CoverageRatio:       formatCoverageRatio(rep.Safety.QueryLogCoveredDays, rep.Safety.RequiredWindowDays),
		APIUserQueries:      formatInt(rep.Safety.APIUserQueries),
		RuleEvalQueries:     formatInt(rep.Safety.RuleEvalQueries),
		ExcludedQueries:     formatInt(rep.Safety.ExcludedQueries),
		TotalFamilies:       formatInt(rep.Summary.TotalFamilies),
		TotalActiveSeries:   formatInt(rep.Summary.TotalActiveSeries),
		DroppableFamilies:   formatInt(rep.Summary.DroppableFamilies),
		DroppableSeries:     formatInt(rep.Summary.DroppableSeries),
		DroppablePercent:    fmt.Sprintf("%.1f", rep.Summary.DroppablePercent),
		DroppableWidth:      fmt.Sprintf("%.1f", width),
		ProtectedFamilies:   formatInt(rep.Summary.ProtectedFamilies),
		ActiveTranche:       rep.Summary.ActiveTranche,
		TotalTranches:       totalTranches,
		SynthesizedFamilies: formatInt(rep.Summary.SynthesizedFamilies),
		SynthesizedSeries:   formatInt(rep.Summary.SynthesizedSeries),
		SynthesizedPercent:  fmt.Sprintf("%.1f", rep.Summary.SynthesizedPercent),
		MaxDropPerTranche:   fmt.Sprintf("%.1f", rep.Summary.MaxDropPercentPerTranche),
		ActiveDropWatch:     formatInt(rep.Summary.ActiveDropWatchCount),
		WildcardSummary:     formatWildcardSummary(rep.WildcardSources),
		BlockReasons:        rep.Safety.BlockReasons,
		Warnings:            rep.Safety.Warnings,
		UnresolvedRegrets:   rep.Safety.UnresolvedRegrets,
		PendingRevertText:   strings.Join(rep.Safety.PendingRevertFamilies, ", "),
		DropsHeading:        dropsHeading,
		SynthWarnings:       rep.SynthesizerWarnings,
	}

	for _, r := range rep.Regrets {
		data.Regrets = append(data.Regrets, htmlRegretRow{
			FamilyName:  r.FamilyName,
			HitCount:    r.HitCount,
			Query:       r.Query,
			ActionTaken: r.ActionTaken,
		})
	}
	for _, d := range selectedDrops {
		data.Drops = append(data.Drops, htmlDropRow{
			FamilyName:    d.FamilyName,
			MetricType:    string(d.MetricType),
			Action:        string(d.Action),
			Series:        formatInt(d.Series),
			Reason:        d.Reason,
			TrancheNumber: d.TrancheNumber,
		})
	}
	for _, tr := range rep.Tranches {
		data.Tranches = append(data.Tranches, htmlTrancheRow{
			Number:        tr.Number,
			FamilyCount:   len(tr.Items),
			Series:        formatInt(tr.Series),
			PercentEstate: fmt.Sprintf("%.1f", tr.PercentEstate),
		})
	}
	for _, wc := range rep.WildcardSources {
		data.Wildcards = append(data.Wildcards, htmlWildcardRow{
			SourceName:         wc.SourceName,
			Origin:             string(wc.Origin),
			Matched:            fmt.Sprintf("%d/%d", wc.MatchedFamilies, wc.TotalFamilies),
			MatchPercent:       fmt.Sprintf("%.1f", wc.MatchPercent),
			Excluded:           wc.Excluded,
			RecommendedExclude: wc.RecommendedExclude,
		})
	}
	for _, dr := range rep.DeadRecordingRules {
		note := dr.Note
		if note == "" {
			if dr.HasOrVectorZero {
				note = "keeps emitting zeros until deleted in rule file"
			} else {
				note = "delete in rule file"
			}
		}
		data.DeadRules = append(data.DeadRules, htmlDeadRuleRow{
			RuleName:        dr.RuleName,
			File:            dr.File,
			Group:           dr.Group,
			HasOrVectorZero: dr.HasOrVectorZero,
			Note:            note,
		})
	}
	for _, adv := range rep.LabelCollapseAdvisories {
		if adv.CombinedEstimate != nil {
			est := adv.CombinedEstimate
			savings := fmt.Sprintf("-%.1f%%", est.SavingsPercent)
			if est.SavingsPercent == 0 {
				savings = "0.0% (no collapse)"
			}
			data.CollapseRows = append(data.CollapseRows, htmlCollapseRow{
				TargetMetric: adv.TargetMetric,
				Labels:       strings.Join(est.DroppedLabels, ", "),
				Current:      formatInt(est.CurrentSeries),
				Collapsed:    formatInt(est.CollapsedSeries),
				Savings:      savings,
				IsZero:       est.SavingsPercent == 0,
			})
		}
		for _, s := range adv.SingleLabelEstimates {
			if adv.CombinedEstimate != nil && len(adv.CombinedEstimate.DroppedLabels) == 1 {
				continue
			}
			if s.SavingsPercent == 0 {
				data.CollapseRows = append(data.CollapseRows, htmlCollapseRow{
					TargetMetric: adv.TargetMetric,
					Labels:       strings.Join(s.DroppedLabels, ", ") + " (alone)",
					Current:      formatInt(s.CurrentSeries),
					Collapsed:    formatInt(s.CollapsedSeries),
					Savings:      "0.0% (no collapse)",
					IsZero:       true,
				})
				break
			}
		}
	}
	for _, ov := range rep.OversizedFamilies {
		data.Oversized = append(data.Oversized, htmlAdvisoryRow{
			FamilyName: ov.FamilyName,
			Series:     formatInt(ov.Series),
			Note:       ov.AdvisoryNote,
		})
	}
	for _, ca := range rep.CapabilityAdvisories {
		data.CapabilityNotes = append(data.CapabilityNotes, htmlAdvisoryRow{
			FamilyName: ca.FamilyName,
			Series:     formatInt(ca.Series),
			Note:       ca.AdvisoryNote,
		})
	}

	var buf bytes.Buffer
	if err := htmlReportTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
