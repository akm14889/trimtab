package promql

import (
	"fmt"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

var defaultParser = parser.NewParser(parser.Options{
	EnableExperimentalFunctions: true,
	EnableBinopFillModifiers:    true,
	EnableUnquotedUTF8Names:     true,
})

// MetricAccess captures a metric selector and its label-usage semantics extracted from a PromQL AST.
type MetricAccess struct {
	MetricName     string
	MetricRegex    string
	IsRegex        bool
	ExplicitLabels []string
	PreservesAll   bool
	WithoutLabels  []string
}

// ExtractResult holds all metric accesses extracted from a single PromQL expression.
type ExtractResult struct {
	Accesses []MetricAccess
}

type evalContext struct {
	preservesAll   bool
	withoutSet     map[string]struct{}
	explicitLabels map[string]struct{}
}

func rootContext() evalContext {
	return evalContext{
		preservesAll:   true,
		explicitLabels: make(map[string]struct{}),
	}
}

func (c evalContext) clone() evalContext {
	out := evalContext{
		preservesAll:   c.preservesAll,
		explicitLabels: make(map[string]struct{}, len(c.explicitLabels)),
	}
	for k := range c.explicitLabels {
		out.explicitLabels[k] = struct{}{}
	}
	if len(c.withoutSet) > 0 {
		out.withoutSet = make(map[string]struct{}, len(c.withoutSet))
		for k := range c.withoutSet {
			out.withoutSet[k] = struct{}{}
		}
	}
	return out
}

func (c *evalContext) addExplicit(names ...string) {
	if c.explicitLabels == nil {
		c.explicitLabels = make(map[string]struct{})
	}
	for _, n := range names {
		if n != "" && n != "__name__" {
			c.explicitLabels[n] = struct{}{}
		}
	}
}

// Extract parses exprStr using the official Prometheus PromQL parser and returns
// every metric selector along with its full label-usage semantics (Invariant #8).
func Extract(exprStr string) (*ExtractResult, error) {
	expr, err := defaultParser.ParseExpr(exprStr)
	if err != nil {
		return nil, fmt.Errorf("parse PromQL %q: %w", exprStr, err)
	}
	res := &ExtractResult{}
	walkExpr(expr, rootContext(), res)
	return res, nil
}

func walkExpr(node parser.Expr, ctx evalContext, res *ExtractResult) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parser.VectorSelector:
		extractVectorSelector(n, ctx, res)

	case *parser.MatrixSelector:
		walkExpr(n.VectorSelector, ctx, res)

	case *parser.SubqueryExpr:
		walkExpr(n.Expr, ctx, res)

	case *parser.ParenExpr:
		walkExpr(n.Expr, ctx, res)

	case *parser.UnaryExpr:
		walkExpr(n.Expr, ctx, res)

	case *parser.StepInvariantExpr:
		walkExpr(n.Expr, ctx, res)

	case *parser.AggregateExpr:
		walkAggregate(n, ctx, res)

	case *parser.BinaryExpr:
		walkBinary(n, ctx, res)

	case *parser.Call:
		walkCall(n, ctx, res)

	case *parser.NumberLiteral, *parser.StringLiteral:
		// No metric references.
	}
}

func walkAggregate(n *parser.AggregateExpr, ctx evalContext, res *ExtractResult) {
	if n.Param != nil {
		walkExpr(n.Param, rootContext(), res)
	}
	// topk / bottomk / limitk / limit_ratio do NOT strip labels from selected series.
	switch n.Op {
	case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
		childCtx := ctx.clone()
		if !n.Without {
			childCtx.addExplicit(n.Grouping...)
		}
		walkExpr(n.Expr, childCtx, res)
		return
	}

	childCtx := ctx.clone()
	if n.Without {
		// sum without (pod) preserves ALL labels except `pod`.
		if childCtx.preservesAll {
			childCtx.preservesAll = false
			childCtx.withoutSet = make(map[string]struct{}, len(n.Grouping))
			for _, g := range n.Grouping {
				childCtx.withoutSet[g] = struct{}{}
			}
		} else if len(childCtx.withoutSet) > 0 {
			for _, g := range n.Grouping {
				childCtx.withoutSet[g] = struct{}{}
			}
		}
		// If outer already collapsed with `by (...)`, keep outer's reduced explicit labels.
	} else {
		// sum by (a, b) or bare sum(...) collapses all labels outside Grouping.
		childCtx.preservesAll = false
		childCtx.withoutSet = nil
		childCtx.addExplicit(n.Grouping...)
	}
	walkExpr(n.Expr, childCtx, res)
}

func walkBinary(n *parser.BinaryExpr, ctx evalContext, res *ExtractResult) {
	lhsCtx := ctx.clone()
	rhsCtx := ctx.clone()

	if vm := n.VectorMatching; vm != nil {
		if vm.On {
			// on (a, b) explicitly uses a and b on both operands.
			lhsCtx.addExplicit(vm.MatchingLabels...)
			rhsCtx.addExplicit(vm.MatchingLabels...)
		} else if len(vm.MatchingLabels) > 0 {
			// ignoring (c) matches on ALL labels except c on both operands.
			applyWithoutMatching(&lhsCtx, vm.MatchingLabels)
			applyWithoutMatching(&rhsCtx, vm.MatchingLabels)
		} else {
			// Default vector-to-vector matching aligns on ALL labels of both operands.
			lhsCtx.preservesAll = true
			rhsCtx.preservesAll = true
		}

		// group_left(extra...) pulls extra labels from RHS; group_right(extra...) pulls from LHS.
		switch vm.Card {
		case parser.CardManyToOne:
			rhsCtx.addExplicit(vm.Include...)
		case parser.CardOneToMany:
			lhsCtx.addExplicit(vm.Include...)
		default:
			lhsCtx.addExplicit(vm.Include...)
			rhsCtx.addExplicit(vm.Include...)
		}
	}

	walkExpr(n.LHS, lhsCtx, res)
	walkExpr(n.RHS, rhsCtx, res)
}

func applyWithoutMatching(ctx *evalContext, ignored []string) {
	if ctx.preservesAll {
		return
	}
	// Vector matching itself needs every label except `ignored`.
	if len(ctx.withoutSet) == 0 {
		ctx.withoutSet = make(map[string]struct{}, len(ignored))
		for _, l := range ignored {
			ctx.withoutSet[l] = struct{}{}
		}
		return
	}
	// Union of used sets = intersection of excluded sets.
	ignoredMap := make(map[string]struct{}, len(ignored))
	for _, l := range ignored {
		ignoredMap[l] = struct{}{}
	}
	for l := range ctx.withoutSet {
		if _, bothIgnored := ignoredMap[l]; !bothIgnored {
			delete(ctx.withoutSet, l)
		}
	}
	if len(ctx.withoutSet) == 0 {
		ctx.preservesAll = true
	}
}

func walkCall(n *parser.Call, ctx evalContext, res *ExtractResult) {
	if n.Func == nil {
		for _, arg := range n.Args {
			walkExpr(arg, ctx.clone(), res)
		}
		return
	}

	switch n.Func.Name {
	case "label_replace":
		// label_replace(v, dst_label, replacement, src_label, regex)
		childCtx := ctx.clone()
		if len(n.Args) >= 5 {
			if dst, ok := stringLiteral(n.Args[1]); ok {
				childCtx.addExplicit(dst)
			}
			if src, ok := stringLiteral(n.Args[3]); ok {
				childCtx.addExplicit(src)
			}
		}
		if len(n.Args) >= 1 {
			walkExpr(n.Args[0], childCtx, res)
		}
		return

	case "label_join":
		// label_join(v, dst_label, separator, src_label_1, src_label_2, ...)
		childCtx := ctx.clone()
		if len(n.Args) >= 4 {
			if dst, ok := stringLiteral(n.Args[1]); ok {
				childCtx.addExplicit(dst)
			}
			for i := 3; i < len(n.Args); i++ {
				if src, ok := stringLiteral(n.Args[i]); ok {
					childCtx.addExplicit(src)
				}
			}
		}
		if len(n.Args) >= 1 {
			walkExpr(n.Args[0], childCtx, res)
		}
		return

	case "histogram_quantile":
		childCtx := ctx.clone()
		childCtx.addExplicit("le")
		for _, arg := range n.Args {
			walkExpr(arg, childCtx, res)
		}
		return

	case "absent", "absent_over_time":
		// absent() does not propagate arbitrary labels from input series, only selector matchers.
		childCtx := evalContext{explicitLabels: make(map[string]struct{})}
		for _, arg := range n.Args {
			walkExpr(arg, childCtx, res)
		}
		return

	default:
		for _, arg := range n.Args {
			walkExpr(arg, ctx.clone(), res)
		}
	}
}

func stringLiteral(e parser.Expr) (string, bool) {
	sl, ok := e.(*parser.StringLiteral)
	if !ok {
		return "", false
	}
	return sl.Val, true
}

func extractVectorSelector(n *parser.VectorSelector, ctx evalContext, res *ExtractResult) {
	explicit := make(map[string]struct{}, len(ctx.explicitLabels))
	for l := range ctx.explicitLabels {
		explicit[l] = struct{}{}
	}

	var metricName string
	var metricRegex string
	var isRegex bool

	if n.Name != "" {
		metricName = n.Name
	}

	for _, m := range n.LabelMatchers {
		if m.Name == labels.MetricName {
			switch m.Type {
			case labels.MatchEqual:
				metricName = m.Value
			case labels.MatchRegexp:
				metricRegex = m.Value
				isRegex = true
			case labels.MatchNotEqual, labels.MatchNotRegexp:
				// Negative matchers on __name__ match almost everything; represent as wildcard regex.
				if metricName == "" && !isRegex {
					metricRegex = ".+"
					isRegex = true
				}
			}
		} else if m.Name != "" {
			explicit[m.Name] = struct{}{}
		}
	}

	if metricName == "" && !isRegex {
		// Bare selector without __name__ (e.g. {job="api"}) matches every metric family.
		metricRegex = ".+"
		isRegex = true
	}

	explicitSlice := make([]string, 0, len(explicit))
	for l := range explicit {
		explicitSlice = append(explicitSlice, l)
	}
	sort.Strings(explicitSlice)

	var withoutSlice []string
	if !ctx.preservesAll && len(ctx.withoutSet) > 0 {
		withoutSlice = make([]string, 0, len(ctx.withoutSet))
		for l := range ctx.withoutSet {
			withoutSlice = append(withoutSlice, l)
		}
		sort.Strings(withoutSlice)
	}

	res.Accesses = append(res.Accesses, MetricAccess{
		MetricName:     metricName,
		MetricRegex:    metricRegex,
		IsRegex:        isRegex,
		ExplicitLabels: explicitSlice,
		PreservesAll:   ctx.preservesAll,
		WithoutLabels:  withoutSlice,
	})
}
