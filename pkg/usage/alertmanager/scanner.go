package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
)

// ScanResult holds all alert labels referenced by Alertmanager routing, inhibit rules,
// and receiver Go templates so alerted metrics never drop routing/grouping labels (Invariant #8).
type ScanResult struct {
	ProtectedLabels    []string
	PreservesAllLabels bool
	MatcherCount       int
	FilesScanned       int
	Warnings           []string
}

var (
	validLabelIdentRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	amMatcherLabelRe  = regexp.MustCompile(`^\s*(?:"([a-zA-Z_][a-zA-Z0-9_]*)"|([a-zA-Z_][a-zA-Z0-9_]*))\s*(?:=~|!~|!=|=)`)
	amTplDotLabelRe   = regexp.MustCompile(`(?:\.(?:CommonLabels|GroupLabels|Labels)|\$labels)\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	amTplIndexLabelRe = regexp.MustCompile(`(?:index\s+(?:\.(?:CommonLabels|GroupLabels|Labels)|\$labels)\s+|(?:\.(?:CommonLabels|GroupLabels|Labels)|\$labels)\s*\[\s*)["']([a-zA-Z_][a-zA-Z0-9_]*)["']`)
)

// Scan reads Alertmanager YAML files/globs and/or GET /api/v2/status and returns protected alert labels.
func Scan(ctx context.Context, cfg *config.AlertmanagerConfig) (*ScanResult, error) {
	res := &ScanResult{}
	if cfg == nil {
		return res, nil
	}

	labelSet := make(map[string]struct{})

	if len(cfg.ConfigPaths) > 0 {
		files, err := expandYAMLFiles(cfg.ConfigPaths)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("read alertmanager config %s: %w", f, err)
			}
			res.FilesScanned++
			if err := parseDocumentBytes(data, labelSet, res); err != nil {
				return nil, fmt.Errorf("parse alertmanager config %s: %w", f, err)
			}
		}
	}

	if cfg.URL != "" {
		if err := scanHTTP(ctx, cfg, labelSet, res); err != nil {
			return nil, err
		}
	}

	res.ProtectedLabels = sortedKeys(labelSet)
	return res, nil
}

// ParseYAML parses raw Alertmanager YAML (including multi-doc files and AlertmanagerConfig CRDs).
func ParseYAML(data []byte) (*ScanResult, error) {
	res := &ScanResult{}
	labelSet := make(map[string]struct{})
	if err := parseDocumentBytes(data, labelSet, res); err != nil {
		return nil, err
	}
	res.ProtectedLabels = sortedKeys(labelSet)
	return res, nil
}

type amStatusResponse struct {
	Config struct {
		Original string `json:"original"`
	} `json:"config"`
}

func scanHTTP(ctx context.Context, cfg *config.AlertmanagerConfig, labelSet map[string]struct{}, res *ScanResult) error {
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		return err
	}
	client, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     timeout,
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		return fmt.Errorf("alertmanager client: %w", err)
	}
	res.Warnings = append(res.Warnings, client.Warnings()...)

	body, err := client.Get(ctx, "/api/v2/status")
	if err != nil {
		return fmt.Errorf("alertmanager %w", err)
	}
	var status amStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return fmt.Errorf("decode alertmanager /api/v2/status: %w", err)
	}
	if strings.TrimSpace(status.Config.Original) == "" {
		return fmt.Errorf("alertmanager /api/v2/status returned empty config.original")
	}
	if err := parseDocumentBytes([]byte(status.Config.Original), labelSet, res); err != nil {
		return fmt.Errorf("parse alertmanager live config.original: %w", err)
	}
	return nil
}

func expandYAMLFiles(patterns []string) ([]string, error) {
	seen := make(map[string]struct{})
	var files []string
	addFile := func(p string) {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			files = append(files, p)
		}
	}

	for _, pat := range patterns {
		if strings.TrimSpace(pat) == "" {
			continue
		}
		info, err := os.Stat(pat)
		if err == nil && info.IsDir() {
			err := filepath.WalkDir(pat, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				lower := strings.ToLower(d.Name())
				if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
					addFile(path)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walk alertmanager dir %s: %w", pat, err)
			}
			continue
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid alertmanager glob %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("alertmanager config path matched no files: %q", pat)
		}
		for _, m := range matches {
			if minfo, sErr := os.Stat(m); sErr == nil && minfo.IsDir() {
				continue
			}
			addFile(m)
		}
	}
	sort.Strings(files)
	return files, nil
}

func parseDocumentBytes(data []byte, labelSet map[string]struct{}, res *ScanResult) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if doc == nil {
			continue
		}
		m := asStringMap(doc)
		if m != nil {
			scanObject(m, labelSet, res)
		}
	}
	return nil
}

func scanObject(doc map[string]any, labelSet map[string]struct{}, res *ScanResult) {
	kind, _ := doc["kind"].(string)
	if kind == "List" || strings.HasSuffix(kind, "List") {
		if items, ok := doc["items"].([]any); ok {
			for _, item := range items {
				if m := asStringMap(item); m != nil {
					scanObject(m, labelSet, res)
				}
			}
		}
		return
	}

	target := doc
	if kind == "AlertmanagerConfig" {
		if spec := asStringMap(doc["spec"]); spec != nil {
			target = spec
		}
	}

	if route := asStringMap(target["route"]); route != nil {
		extractRoute(route, labelSet, res)
	}
	extractInhibitRules(target["inhibit_rules"], labelSet, res)
	extractInhibitRules(target["inhibitRules"], labelSet, res)
	extractReceivers(target["receivers"], labelSet, res)
}

func extractRoute(route map[string]any, labelSet map[string]struct{}, res *ScanResult) {
	addGroupBy := func(raw any) {
		items, ok := raw.([]any)
		if !ok {
			return
		}
		for _, it := range items {
			lbl, ok := it.(string)
			if !ok {
				continue
			}
			lbl = strings.TrimSpace(lbl)
			if lbl == "..." {
				res.PreservesAllLabels = true
				res.MatcherCount++
				continue
			}
			if isValidAlertLabel(lbl) {
				labelSet[lbl] = struct{}{}
				res.MatcherCount++
			}
		}
	}
	addGroupBy(route["group_by"])
	addGroupBy(route["groupBy"])

	addMatchMap(route["match"], labelSet, res)
	addMatchMap(route["match_re"], labelSet, res)
	addMatcherList(route["matchers"], labelSet, res)

	if subRoutes, ok := route["routes"].([]any); ok {
		for _, sub := range subRoutes {
			if subMap := asStringMap(sub); subMap != nil {
				extractRoute(subMap, labelSet, res)
			}
		}
	}
}

func extractInhibitRules(raw any, labelSet map[string]struct{}, res *ScanResult) {
	rules, ok := raw.([]any)
	if !ok {
		return
	}
	for _, r := range rules {
		rm := asStringMap(r)
		if rm == nil {
			continue
		}
		addMatchMap(rm["source_match"], labelSet, res)
		addMatchMap(rm["source_match_re"], labelSet, res)
		addMatchMap(rm["target_match"], labelSet, res)
		addMatchMap(rm["target_match_re"], labelSet, res)
		addMatchMap(rm["sourceMatch"], labelSet, res)
		addMatchMap(rm["targetMatch"], labelSet, res)
		addMatcherList(rm["source_matchers"], labelSet, res)
		addMatcherList(rm["target_matchers"], labelSet, res)
		addMatcherList(rm["sourceMatchers"], labelSet, res)
		addMatcherList(rm["targetMatchers"], labelSet, res)

		if eqList, ok := rm["equal"].([]any); ok {
			for _, it := range eqList {
				if lbl, ok := it.(string); ok {
					lbl = strings.TrimSpace(lbl)
					if isValidAlertLabel(lbl) {
						labelSet[lbl] = struct{}{}
						res.MatcherCount++
					}
				}
			}
		}
	}
}

func addMatchMap(raw any, labelSet map[string]struct{}, res *ScanResult) {
	m := asStringMap(raw)
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if isValidAlertLabel(k) {
			labelSet[k] = struct{}{}
			res.MatcherCount++
		}
	}
}

func addMatcherList(raw any, labelSet map[string]struct{}, res *ScanResult) {
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for _, it := range items {
		switch v := it.(type) {
		case string:
			if m := amMatcherLabelRe.FindStringSubmatch(v); len(m) >= 3 {
				lbl := m[1]
				if lbl == "" {
					lbl = m[2]
				}
				if isValidAlertLabel(lbl) {
					labelSet[lbl] = struct{}{}
					res.MatcherCount++
				}
			}
		default:
			if mm := asStringMap(it); mm != nil {
				if name, ok := mm["name"].(string); ok && isValidAlertLabel(strings.TrimSpace(name)) {
					labelSet[strings.TrimSpace(name)] = struct{}{}
					res.MatcherCount++
				}
			}
		}
	}
}

func extractReceivers(raw any, labelSet map[string]struct{}, res *ScanResult) {
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for _, it := range items {
		recLabels := make(map[string]struct{})
		walkTemplateStrings(it, recLabels)
		for lbl := range recLabels {
			labelSet[lbl] = struct{}{}
			res.MatcherCount++
		}
	}
}

func walkTemplateStrings(v any, out map[string]struct{}) {
	switch val := v.(type) {
	case string:
		for _, m := range amTplDotLabelRe.FindAllStringSubmatch(val, -1) {
			if len(m) > 1 && isTemplateLabelCandidate(m[1]) {
				out[m[1]] = struct{}{}
			}
		}
		for _, m := range amTplIndexLabelRe.FindAllStringSubmatch(val, -1) {
			if len(m) > 1 && isValidAlertLabel(m[1]) {
				out[m[1]] = struct{}{}
			}
		}
	case []any:
		for _, elem := range val {
			walkTemplateStrings(elem, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkTemplateStrings(val[k], out)
		}
	case map[any]any:
		for _, elem := range val {
			walkTemplateStrings(elem, out)
		}
	}
}

func isValidAlertLabel(s string) bool {
	return s != "" && s != "__name__" && validLabelIdentRe.MatchString(s)
}

func isTemplateLabelCandidate(s string) bool {
	if !isValidAlertLabel(s) {
		return false
	}
	// Ignore Go template Alertmanager KV helper methods such as `.CommonLabels.SortedPairs`.
	switch s {
	case "SortedPairs", "Names", "Values":
		return false
	default:
		return true
	}
}

func asStringMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}
		return out
	default:
		return nil
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" && k != "__name__" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
