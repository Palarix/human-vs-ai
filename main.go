// human-vs-ai estimates the human replacement effort represented by a Git
// repository and compares it with an explicit AI-assisted cost scenario.
package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// --- COLORS ---
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	cyan   = "\033[36m"
	yellow = "\033[33m"
	green  = "\033[32m"
	red    = "\033[31m"
	white  = "\033[97m"
)

func complexityColor(v float64) string {
	switch {
	case v <= 0.6:
		return dim
	case v <= 1.2:
		return white
	case v <= 1.5:
		return yellow
	default:
		return red
	}
}

// --- CONFIGURATION ---
type region struct {
	label string
	rate  float64
}

var regions = map[string]region{
	"--us":   {"US", 100.0},
	"--eu":   {"Western Europe", 75.0},
	"--east": {"Eastern Europe", 45.0},
	"--asia": {"Asia", 30.0},
}

// rateMult: applied to region hourly rate
// locMult:  applied to baseLOCPerHour (higher = faster output = fewer hours billed)
// Net effect: principal costs more per commit because rateMult > locMult
type seniority struct {
	label    string
	rateMult float64
	locMult  float64
}

var seniorities = map[string]seniority{
	"--junior":    {"Junior", 0.6, 0.7},
	"--senior":    {"Senior", 1.0, 1.0},
	"--principal": {"Principal", 1.75, 1.35},
}

// Explicit float64() conversions around products stop the compiler from fusing
// a*b+c into FMA instructions (arm64, ppc64, s390x), which would change
// rounding versus other platforms and the original Ruby implementation.

type estimationConfig struct {
	baseLOCPerHour float64
	deletionWeight float64
}

var estimationProfiles = map[string]estimationConfig{
	"low":  {40, 0.05},
	"base": {25, 0.10},
	"high": {15, 0.20},
}

type aiTokenModel struct {
	tokensPerEffortUnit  float64
	uncachedInputShare   float64
	cachedInputShare     float64
	outputShare          float64
	uncachedInputPerMTok float64
	cachedInputPerMTok   float64
	outputPerMTok        float64
}

// A medium-effort, frontier coding-agent scenario using Sol-level pricing.
// Token volume includes repository reads, tool output, retries, and reasoning;
// it is intentionally much larger than the tokens in the final code alone.
var defaultAITokenModel = aiTokenModel{
	tokensPerEffortUnit:  2500,
	uncachedInputShare:   0.30,
	cachedInputShare:     0.50,
	outputShare:          0.20,
	uncachedInputPerMTok: 2.00,
	cachedInputPerMTok:   0.20,
	outputPerMTok:        10.00,
}

// 1.0 = Standard (Ruby/Python/Go) | < 1.0 = Fast (CSS/MD) | > 1.0 = Slow (C/C++/Rust)
var complexityMap = map[string]float64{
	// Web / scripting
	".rb": 1.0, ".py": 1.0, ".php": 0.9, ".sh": 0.8,
	// JavaScript / TypeScript
	".js": 1.0, ".jsx": 1.0, ".ts": 1.3, ".tsx": 1.3,
	// Systems languages
	".rs": 2.2, ".cpp": 2.0, ".c": 1.6, ".go": 1.0,
	// JVM / mobile
	".java": 1.4, ".kt": 1.1, ".scala": 1.6, ".swift": 1.3, ".cs": 1.3,
	// Functional
	".ex": 1.1, ".exs": 1.1, ".hs": 2.0,
	// Query / schema
	".sql": 0.9, ".graphql": 0.5, ".gql": 0.5, ".proto": 0.4,
	// Markup / templates
	".html": 0.6, ".erb": 0.7, ".haml": 0.6, ".slim": 0.6,
	// Styles
	".css": 0.5, ".scss": 0.5, ".sass": 0.5,
	// Config / data
	".yml": 0.3, ".yaml": 0.3, ".toml": 0.2, ".json": 0.2, ".xml": 0.5,
	// Infra
	".tf": 0.6,
	// Docs
	".md": 0.3, ".txt": 0.2, ".rst": 0.3,
	// Lock files (machine-generated)
	".lock": 0.05,
	// Text SVG remains measurable; binary formats contribute no line effort.
	".svg": 0.3, ".png": 0.0, ".jpg": 0.0, ".jpeg": 0.0,
	".gif": 0.0, ".webp": 0.0, ".ico": 0.0, ".woff": 0.0, ".woff2": 0.0,
}

// Change type rules: checked in order, first match wins.
// Multiplier scales human effort — 1.0 = manual, 0.0 = generated output.
type changeTypeRule struct {
	label      string
	match      func(path string) bool
	multiplier float64
}

var (
	// Build output directories
	reBuildDir = regexp.MustCompile(`(?:\A|/)(?:dist|build|out|\.next|_next|_site|\.nuxt|public/build|static/chunks|\.svelte-kit|coverage)/`)
	// Minified / bundled files
	reBundled = regexp.MustCompile(`(?i)\.(min\.js|min\.css|bundle\.js|chunk\.js|map)\z`)
	// Exported binary assets
	reAsset = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp|ico|woff2?|eot|ttf|otf)\z`)

	reDepFile = regexp.MustCompile(`\A(Gemfile\.lock|package-lock\.json|yarn\.lock|Cargo\.lock|Pipfile\.lock|composer\.lock|.*\.lock|Gemfile|package\.json|Cargo\.toml|go\.mod|go\.sum)\z`)
	reTest    = regexp.MustCompile(`(?i)(?:\A|/)(?:test|tests|spec|__tests__)/|(_spec\.|_test\.|\.test\.|\.spec\.)`)
	reDocs    = regexp.MustCompile(`(?i)\.(md|txt|rst|adoc)\z`)
	reConfig  = regexp.MustCompile(`(?i)\.(yml|yaml|toml|xml|json|lock|tf)\z`)
)

var changeTypeRules = []changeTypeRule{
	{
		label: "generated",
		match: func(p string) bool {
			return reBuildDir.MatchString(p) || reBundled.MatchString(p) || reAsset.MatchString(p)
		},
		multiplier: 0.0,
	},
	{label: "dep-update", match: func(p string) bool { return reDepFile.MatchString(filepath.Base(p)) }, multiplier: 0.1},
	{label: "test", match: reTest.MatchString, multiplier: 1.0},
	{label: "docs", match: reDocs.MatchString, multiplier: 1.0},
	{label: "config", match: reConfig.MatchString, multiplier: 1.0},
	{label: "std", match: func(string) bool { return true }, multiplier: 1.0},
}

func changeTypeFor(path string) changeTypeRule {
	for _, r := range changeTypeRules {
		if r.match(path) {
			return r
		}
	}
	return changeTypeRules[len(changeTypeRules)-1]
}

// extname mirrors Ruby's File.extname: leading dots of the basename are not
// an extension, so ".bashrc" has none.
func extname(path string) string {
	return filepath.Ext(strings.TrimLeft(filepath.Base(path), "."))
}

// counter is an insertion-ordered tally, so ties sort deterministically.
type counter struct {
	keys []string
	vals map[string]int
}

func newCounter() *counter { return &counter{vals: map[string]int{}} }

func (c *counter) add(k string, n int) {
	if _, ok := c.vals[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.vals[k] += n
}

// sortedDesc returns keys ordered by value, largest first.
func (c *counter) sortedDesc(val func(string) int) []string {
	keys := append([]string(nil), c.keys...)
	sort.SliceStable(keys, func(i, j int) bool { return val(keys[i]) > val(keys[j]) })
	return keys
}

// filePatchStat is one file's contribution to a commit diff.
type filePatchStat struct {
	path      string
	additions int
	deletions int
}

type changeTypeInfo struct {
	blendedMultiplier float64
	breakdown         *counter // label => pct
}

func getChangeTypeInfo(patches []filePatchStat, deletionWeight float64) changeTypeInfo {
	unitsByType := map[string]float64{}
	var labels []string
	multByType := map[string]float64{}
	totalUnits := 0.0

	for _, p := range patches {
		units := float64(p.additions) + float64(p.deletions)*deletionWeight
		if units == 0 {
			continue
		}
		rule := changeTypeFor(p.path)
		if _, ok := unitsByType[rule.label]; !ok {
			labels = append(labels, rule.label)
		}
		unitsByType[rule.label] += units
		multByType[rule.label] = rule.multiplier
		totalUnits += units
	}

	if totalUnits == 0 {
		b := newCounter()
		b.add("std", 100)
		return changeTypeInfo{1.0, b}
	}

	weightedSum := 0.0
	breakdown := newCounter()
	type remainder struct {
		label string
		frac  float64
	}
	var remainders []remainder
	allocated := 0
	for _, label := range labels {
		units := unitsByType[label]
		weightedSum += multByType[label] * units
		exact := units * 100 / totalUnits
		whole := int(math.Floor(exact))
		breakdown.add(label, whole)
		allocated += whole
		remainders = append(remainders, remainder{label, exact - float64(whole)})
	}
	sort.SliceStable(remainders, func(i, j int) bool { return remainders[i].frac > remainders[j].frac })
	for i := 0; i < 100-allocated; i++ {
		breakdown.vals[remainders[i].label]++
	}
	return changeTypeInfo{weightedSum / totalUnits, breakdown}
}

type complexityInfo struct {
	blendedMultiplier float64
	byLang            *counter
}

// Complexity is effort-delta-weighted across patches (not patch-count-weighted).
func getComplexityInfo(patches []filePatchStat, deletionWeight float64) complexityInfo {
	weightedSum := 0.0
	totalUnits := 0.0
	byLang := newCounter()

	for _, p := range patches {
		units := float64(p.additions) + float64(p.deletions)*deletionWeight
		if units == 0 {
			continue
		}
		ext := extname(p.path)
		lang := ext
		if ext == "" {
			lang = "(none)"
		}
		byLang.add(lang, int(math.Round(units)))
		mult, ok := complexityMap[ext]
		if !ok {
			mult = 1.0
		}
		weightedSum += mult * units
		totalUnits += units
	}

	blended := 1.0
	if totalUnits > 0 {
		blended = weightedSum / totalUnits
	}
	return complexityInfo{blended, byLang}
}

// commitPatchStats diffs a commit against its parent with rename detection so
// file moves are not priced as a full deletion plus re-creation.
func commitPatchStats(parent, commit *object.Commit) ([]filePatchStat, error) {
	from := &object.Tree{}
	var err error
	if parent != nil {
		from, err = parent.Tree()
		if err != nil {
			return nil, err
		}
	}
	to, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTreeWithOptions(context.Background(), from, to, object.DefaultDiffTreeOptions)
	if err != nil {
		return nil, err
	}

	stats := make([]filePatchStat, 0, len(changes))
	for _, change := range changes {
		// Like libgit2's new_file path: falls back to the old path on deletion.
		s := filePatchStat{path: change.To.Name}
		if s.path == "" {
			s.path = change.From.Name
		}
		// Submodule updates yield no files and count as zero lines.
		f, t, err := change.Files()
		if err != nil {
			return nil, err
		}
		a, err := fileContent(f)
		if err != nil {
			return nil, err
		}
		b, err := fileContent(t)
		if err != nil {
			return nil, err
		}
		if !isBinary(a) && !isBinary(b) {
			s.additions, s.deletions = lineDiffStat(a, b)
		}
		stats = append(stats, s)
	}
	return stats, nil
}

func fileContent(f *object.File) ([]byte, error) {
	if f == nil {
		return nil, nil
	}
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

type repoStats struct {
	loc          int
	estimates    map[string]estimateTotals
	merges       int
	commits      int
	firstDate    time.Time
	lastDate     time.Time
	activeMonths map[string]struct{}
}

type estimateTotals struct {
	effortUnits float64
	humanHours  float64
	humanCost   float64
}

type commitEstimate struct {
	weightedDelta       float64
	effortUnits         float64
	effectiveMultiplier float64
	humanHours          float64
	humanCost           float64
}

func estimateCommit(patches []filePatchStat, hourlyRate float64, sen seniority, cfg estimationConfig) commitEstimate {
	var e commitEstimate
	for _, p := range patches {
		delta := float64(p.additions) + float64(p.deletions)*cfg.deletionWeight
		if delta == 0 {
			continue
		}
		langMult, ok := complexityMap[extname(p.path)]
		if !ok {
			langMult = 1
		}
		typeMult := changeTypeFor(p.path).multiplier
		e.weightedDelta += delta
		e.effortUnits += delta * langMult * typeMult
	}
	if e.weightedDelta == 0 {
		return e
	}
	e.effectiveMultiplier = e.effortUnits / e.weightedDelta
	e.humanHours = e.effortUnits / (cfg.baseLOCPerHour * sen.locMult)
	e.humanCost = e.humanHours * hourlyRate
	return e
}

type analysisOptions struct {
	profile      string
	teamSize     int
	apiCostTotal *float64
	reviewHours  *float64
}

func analyzeRepo(w io.Writer, path string, reg region, sen seniority, detail bool, opts analysisOptions) error {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return fmt.Errorf("'%s' is not a valid git repository", path)
	}
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("could not resolve HEAD: %w", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return err
	}
	defer iter.Close()

	stats := repoStats{estimates: map[string]estimateTotals{}, activeMonths: map[string]struct{}{}}

	hourlyRate := reg.rate * sen.rateMult
	selectedCfg := estimationProfiles[opts.profile]

	if detail {
		abs, _ := filepath.Abs(path)
		fmt.Fprintf(w, "\n%s%sAnalyzing: %s%s\n", bold, cyan, abs, reset)
		fmt.Fprintf(w, "%sRegion:   %s%s%s%s%s · %s ($%.0f/hr, %.1f LOC/hr base)%s\n",
			dim, reset, white, reg.label, reset, dim, sen.label, math.Round(hourlyRate), round1(selectedCfg.baseLOCPerHour*sen.locMult), reset)
		fmt.Fprintln(w, dim+fmt.Sprintf("%-12s | %-11s | %-9s | %-15s", "Date", "LOC", "Mult", "HC")+reset)
		fmt.Fprintln(w, dim+strings.Repeat("-", 54)+reset)
	}

	err = iter.ForEach(func(commit *object.Commit) error {
		if commit.NumParents() > 1 {
			stats.merges++
			return nil
		}
		var parent *object.Commit
		if commit.NumParents() == 1 {
			parent, err = commit.Parent(0)
			if err != nil {
				return err
			}
		}
		patches, err := commitPatchStats(parent, commit)
		if err != nil {
			return fmt.Errorf("diffing %s: %w", commit.Hash, err)
		}

		additions, deletions := 0, 0
		for _, p := range patches {
			additions += p.additions
			deletions += p.deletions
		}
		selected := estimateCommit(patches, hourlyRate, sen, selectedCfg)
		if selected.weightedDelta == 0 {
			return nil
		}

		langInfo := getComplexityInfo(patches, selectedCfg.deletionWeight)
		typeInfo := getChangeTypeInfo(patches, selectedCfg.deletionWeight)

		for name, cfg := range estimationProfiles {
			if name == opts.profile {
				cfg = selectedCfg
			}
			e := estimateCommit(patches, hourlyRate, sen, cfg)
			t := stats.estimates[name]
			t.effortUnits += e.effortUnits
			t.humanHours += e.humanHours
			t.humanCost += e.humanCost
			stats.estimates[name] = t
		}

		when := commit.Committer.When
		stats.loc += additions
		stats.commits++
		stats.activeMonths[when.Format("2006-01")] = struct{}{}
		if stats.lastDate.IsZero() || when.After(stats.lastDate) {
			stats.lastDate = when
		}
		if stats.firstDate.IsZero() || when.Before(stats.firstDate) {
			stats.firstDate = when
		}
		if detail {
			fmt.Fprintf(w, "%s%-12s%s • %sLOC:%-7s%s | %sMult:%-4.1f%s | %sHC:$%-11s%s\n",
				dim, when.Format("2006-01-02"), reset,
				yellow, fmtInt(math.Round(selected.weightedDelta)), reset,
				complexityColor(selected.effectiveMultiplier), selected.effectiveMultiplier, reset,
				red, fmtMoney(selected.humanCost), reset)

			var langParts []string
			for _, lang := range langInfo.byLang.sortedDesc(func(k string) int { return langInfo.byLang.vals[k] }) {
				langParts = append(langParts, fmt.Sprintf("%s%s: %d%s", dim, lang, langInfo.byLang.vals[lang], white))
			}
			fmt.Fprintf(w, "%s             ├─ %s%s\n", white, strings.Join(langParts, "  "), reset)

			var typeParts []string
			bd := typeInfo.breakdown
			for _, label := range bd.sortedDesc(func(k string) int { return bd.vals[k] }) {
				typeParts = append(typeParts, fmt.Sprintf("%s%s: %d%%%s", dim, label, bd.vals[label], white))
			}
			fmt.Fprintf(w, "%s             └─ %s%s\n", white, strings.Join(typeParts, "  "), reset)
		}
		return nil
	})
	if err != nil {
		return err
	}

	renderSummary(w, stats, reg, sen, hourlyRate, opts)
	return nil
}

// --- FORMATTING ---

// round1 rounds half away from zero to one decimal, like Ruby's Float#round(1).
func round1(v float64) float64 { return math.Round(v*10) / 10 }

func groupThousands(digits string) string {
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fmtInt truncates toward zero and groups thousands: 16693.4 => "16,693".
func fmtInt(n float64) string {
	i := int64(n)
	if i < 0 {
		return "-" + groupThousands(fmt.Sprint(-i))
	}
	return groupThousands(fmt.Sprint(i))
}

// fmtMoney formats with two decimals and grouped thousands: 79067.781 => "79,067.78".
func fmtMoney(n float64) string {
	s := fmt.Sprintf("%.2f", n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	intPart, dec, _ := strings.Cut(s, ".")
	return sign + groupThousands(intPart) + "." + dec
}

func formatDuration(days float64) string {
	switch {
	case days < 1:
		return "less than a day"
	case days < 30:
		return fmt.Sprintf("%.1f days", round1(days))
	case days < 365:
		return fmt.Sprintf("%.1f months", round1(days/30.0))
	default:
		return fmt.Sprintf("%.1f years", round1(days/365.0))
	}
}

func tableRow(label, value, valueColor string) string {
	return fmt.Sprintf("%s│%s %s%-22s%s %s│%s %s%-28s%s %s│%s",
		dim, reset, dim, label, reset, dim, reset, valueColor, value, reset, dim, reset)
}

func tableHeader(title, subtitle string) string {
	return fmt.Sprintf("%s│%s %s%s%-22s%s %s│%s %s%-28s%s %s│%s",
		dim, reset, bold, cyan, title, reset, dim, reset, dim, subtitle, reset, dim, reset)
}

var (
	tableDivider = dim + "├─" + strings.Repeat("─", 23) + "┼" + strings.Repeat("─", 30) + "┤" + reset
	tableTop     = dim + "┌─" + strings.Repeat("─", 23) + "┬" + strings.Repeat("─", 30) + "┐" + reset
	tableBottom  = dim + "└─" + strings.Repeat("─", 23) + "┴" + strings.Repeat("─", 30) + "┘" + reset
)

func savingsPercent(humanCost, aiCost float64) float64 {
	if humanCost == 0 {
		return 0
	}
	return (humanCost - aiCost) / humanCost * 100
}

func moneyRange(a, b float64) string {
	if a > b {
		a, b = b, a
	}
	return "$" + fmtMoney(a) + "–$" + fmtMoney(b)
}

func humanCostBounds(s repoStats) (minCost, maxCost float64) {
	minCost, maxCost = math.Inf(1), math.Inf(-1)
	for _, name := range []string{"low", "base", "high"} {
		cost := s.estimates[name].humanCost
		minCost, maxCost = math.Min(minCost, cost), math.Max(maxCost, cost)
	}
	return
}

func estimateAITokens(effortUnits float64, model aiTokenModel) (tokens, cost float64) {
	tokens = effortUnits * model.tokensPerEffortUnit
	uncached := tokens * model.uncachedInputShare
	cached := tokens * model.cachedInputShare
	output := tokens * model.outputShare
	cost = uncached/1_000_000*model.uncachedInputPerMTok +
		cached/1_000_000*model.cachedInputPerMTok +
		output/1_000_000*model.outputPerMTok
	return
}

func fmtTokens(tokens float64) string {
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", tokens/1_000_000)
	}
	if tokens >= 1_000 {
		return fmt.Sprintf("%.1fK", tokens/1_000)
	}
	return fmtInt(tokens)
}

func renderSummary(w io.Writer, s repoStats, reg region, sen seniority, hourlyRate float64, opts analysisOptions) {
	selected := s.estimates[opts.profile]
	tokenCount, toolSpend := estimateAITokens(selected.effortUnits, defaultAITokenModel)
	toolLabel := "AI cost (estimated)"
	if opts.apiCostTotal != nil {
		toolSpend = *opts.apiCostTotal
		toolLabel = "AI cost (provided)"
	}
	operatorCost := 0.0
	if opts.reviewHours != nil {
		operatorCost = *opts.reviewHours * hourlyRate
	}
	totalAICost := toolSpend + operatorCost
	savings := savingsPercent(selected.humanCost, totalAICost)
	savingsCol := green
	if savings < 0 {
		savingsCol = red
	}

	p := func(line string) { fmt.Fprintln(w, line) }

	p("")
	p(tableTop)
	p(tableHeader("EFFORT ESTIMATE", fmt.Sprintf("%s · %s · %s", reg.label, sen.label, opts.profile)))
	p(tableDivider)
	p(tableRow("Commits", fmt.Sprintf("%s  (+%s merges skipped)", fmtInt(float64(s.commits)), fmtInt(float64(s.merges))), white))
	p(tableRow("LOC Added", fmtInt(float64(s.loc)), yellow))
	p(tableRow("Human replacement", "$"+fmtMoney(selected.humanCost), red))
	p(tableRow("AI scenario", "Sol-level · medium effort", white))
	p(tableRow("Tokens / weighted LOC", fmtInt(defaultAITokenModel.tokensPerEffortUnit), dim))
	p(tableRow("AI tokens (estimated)", fmtTokens(tokenCount), white))
	p(tableRow(toolLabel, "$"+fmtMoney(toolSpend), green))
	costPerUnit := 0.0
	if selected.effortUnits > 0 {
		costPerUnit = toolSpend / selected.effortUnits
	}
	p(tableRow("AI cost / weighted LOC", fmt.Sprintf("$%.4f", costPerUnit), dim))
	if opts.reviewHours == nil {
		p(tableRow("Operator labor", "not provided", dim))
		p(tableRow("Tool-cost savings", fmt.Sprintf("%.1f%%", round1(savings)), savingsCol))
	} else {
		p(tableRow("Operator cost", "$"+fmtMoney(operatorCost), green))
		p(tableRow("AI-assisted total", "$"+fmtMoney(totalAICost), green))
		p(tableRow("Estimated savings", fmt.Sprintf("%.1f%%", round1(savings)), savingsCol))
	}
	p(tableDivider)
	p(tableHeader("HUMAN SENSITIVITY", "low–high assumptions"))
	p(tableDivider)
	humanMin, humanMax := humanCostBounds(s)
	p(tableRow("Human cost range", moneyRange(humanMin, humanMax), dim))

	if s.commits > 0 {
		actualDays := math.Max(s.lastDate.Sub(s.firstDate).Seconds()/86400.0, 1)
		humanHours := selected.humanHours
		workingDays := humanHours / 8.0

		p(tableDivider)
		p(tableHeader("EFFORT & SPAN", s.firstDate.Format("2006-01-02")+" → "+s.lastDate.Format("2006-01-02")))
		p(tableDivider)
		p(tableRow("Repository span", formatDuration(actualDays), white))
		p(tableRow("Active months", fmt.Sprintf("%d", len(s.activeMonths)), white))
		p(tableRow("Human equivalent", fmt.Sprintf("%s hrs / %s workdays", fmtInt(math.Round(humanHours)), fmtInt(math.Round(workingDays))), white))
		if opts.teamSize > 0 {
			teamDays := (workingDays / float64(opts.teamSize)) * (7.0 / 5)
			p(tableRow("Team capacity floor", fmt.Sprintf("%d people · %s", opts.teamSize, formatDuration(teamDays)), dim))
		}
	}

	p(tableBottom)
}

const usage = `Usage: human-vs-ai <path_to_git_repo> [options]

  Region (default --us):
    --us    US market rate         ($100/hr)
    --eu    Western Europe          ($75/hr)
    --east  Eastern Europe          ($45/hr)
    --asia  Asia                    ($30/hr)

  Seniority (default --senior):
    --junior     0.6x rate · 0.7x LOC/hr
    --senior     1.0x rate · 1.0x LOC/hr
    --principal  1.75x rate · 1.35x LOC/hr

  Output:
    --detail     Show per-commit breakdown (default: summary only)

  Estimation:
    --profile <low|base|high>       Assumption profile (default: base)
    --team-size <n>                 Show a perfect-parallel capacity floor
    --ai-cost-total <amount>        Use known subscription/API spend
    --operator-hours <hours>        Include known human operator time

  General:
    --version    Print version and exit
`

func nextOptionValue(args []string, i *int, name string) (string, error) {
	if value, ok := strings.CutPrefix(args[*i], name+"="); ok {
		return value, nil
	}
	if *i+1 >= len(args) {
		return "", fmt.Errorf("%s requires a value", name)
	}
	*i++
	return args[*i], nil
}

func parseFloatOption(args []string, i *int, name string) (*float64, error) {
	s, err := nextOptionValue(args, i, name)
	if err != nil {
		return nil, err
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, fmt.Errorf("%s must be zero or a positive number", name)
	}
	return &v, nil
}

func main() {
	var repoPath string
	reg, sen, detail := regions["--us"], seniorities["--senior"], false
	opts := analysisOptions{profile: "base"}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--version":
			fmt.Println("human-vs-ai", version)
			return
		case a == "--help" || a == "-h":
			fmt.Print(usage)
			return
		case a == "--detail":
			detail = true
		case regions[a].label != "":
			reg = regions[a]
		case seniorities[a].label != "":
			sen = seniorities[a]
		case a == "--profile" || strings.HasPrefix(a, "--profile="):
			value, err := nextOptionValue(args, &i, "--profile")
			if err != nil {
				fmt.Printf("Error: %s.\n", err)
				return
			}
			if _, ok := estimationProfiles[value]; !ok {
				fmt.Println("Error: --profile must be low, base, or high.")
				return
			}
			opts.profile = value
		case a == "--team-size" || strings.HasPrefix(a, "--team-size="):
			value, err := nextOptionValue(args, &i, "--team-size")
			n, convErr := strconv.Atoi(value)
			if err != nil || convErr != nil || n <= 0 {
				fmt.Println("Error: --team-size must be a positive integer.")
				return
			}
			opts.teamSize = n
		case a == "--ai-cost-total" || strings.HasPrefix(a, "--ai-cost-total=") || a == "--api-cost-total" || strings.HasPrefix(a, "--api-cost-total="):
			name := "--ai-cost-total"
			if strings.HasPrefix(a, "--api-cost-total") {
				name = "--api-cost-total"
			}
			var err error
			opts.apiCostTotal, err = parseFloatOption(args, &i, name)
			if err != nil {
				fmt.Printf("Error: %s.\n", err)
				return
			}
		case a == "--operator-hours" || strings.HasPrefix(a, "--operator-hours=") || a == "--review-hours" || strings.HasPrefix(a, "--review-hours="):
			name := "--operator-hours"
			if strings.HasPrefix(a, "--review-hours") {
				name = "--review-hours"
			}
			var err error
			opts.reviewHours, err = parseFloatOption(args, &i, name)
			if err != nil {
				fmt.Printf("Error: %s.\n", err)
				return
			}
		case strings.HasPrefix(a, "-"):
			fmt.Printf("Error: unknown option %s.\n", a)
			return
		case repoPath == "":
			repoPath = a
		default:
			fmt.Printf("Error: unexpected argument %s.\n", a)
			return
		}
	}

	if repoPath == "" {
		fmt.Print(usage)
		return
	}

	if err := analyzeRepo(os.Stdout, repoPath, reg, sen, detail, opts); err != nil {
		fmt.Printf("Error: %s.\n", strings.TrimSuffix(err.Error(), "."))
		os.Exit(1)
	}
}
