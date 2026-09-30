// human-vs-ai estimates what a git repository would have cost to build with a
// human engineering team versus what it cost to build with AI agents.
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

const (
	baseLOCPerHour       = 25.0
	aiBaseCostPerCommit  = 0.05
	aiHumanOversightMins = 2.0
)

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
	// Assets / images (exported, not hand-written)
	".svg": 0.05, ".png": 0.0, ".jpg": 0.0, ".jpeg": 0.0,
	".gif": 0.0, ".webp": 0.0, ".ico": 0.0, ".woff": 0.0, ".woff2": 0.0,
}

// Change type rules: checked in order, first match wins.
// Multiplier scales human effort — 1.0 = normal, 0.05 = almost free (automated).
type changeTypeRule struct {
	label      string
	match      func(path string) bool
	multiplier float64
}

var (
	// Build output directories
	reBuildDir = regexp.MustCompile(`\A(dist|build|out|\.next|_next|_site|\.nuxt|public/build|static/chunks|\.svelte-kit|coverage)/`)
	// Minified / bundled files
	reBundled = regexp.MustCompile(`(?i)\.(min\.js|min\.css|bundle\.js|chunk\.js|map)\z`)
	// Exported image/vector assets
	reAsset = regexp.MustCompile(`(?i)\.(svg|png|jpe?g|gif|webp|ico|woff2?|eot|ttf|otf)\z`)

	reDepFile = regexp.MustCompile(`\A(Gemfile\.lock|package-lock\.json|yarn\.lock|Cargo\.lock|Pipfile\.lock|composer\.lock|.*\.lock|Gemfile|package\.json|Cargo\.toml|go\.mod|go\.sum)\z`)
	reTest    = regexp.MustCompile(`(?i)(_spec\.|_test\.|\.test\.|\.spec\.)`)
	reDocs    = regexp.MustCompile(`(?i)\.(md|txt|rst|adoc)\z`)
	reConfig  = regexp.MustCompile(`(?i)\.(yml|yaml|toml|xml|json|lock|tf)\z`)
)

var changeTypeRules = []changeTypeRule{
	{
		label: "generated",
		match: func(p string) bool {
			return reBuildDir.MatchString(p) || reBundled.MatchString(p) || reAsset.MatchString(p)
		},
		multiplier: 0.02,
	},
	{label: "dep-update", match: func(p string) bool { return reDepFile.MatchString(filepath.Base(p)) }, multiplier: 0.05},
	{label: "test", match: reTest.MatchString, multiplier: 0.6},
	{label: "docs", match: reDocs.MatchString, multiplier: 0.3},
	{label: "config", match: reConfig.MatchString, multiplier: 0.4},
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

func getChangeTypeInfo(patches []filePatchStat) changeTypeInfo {
	locByType := newCounter()
	multByType := map[string]float64{}
	totalLOC := 0

	for _, p := range patches {
		loc := p.additions + p.deletions
		if loc == 0 {
			continue
		}
		rule := changeTypeFor(p.path)
		locByType.add(rule.label, loc)
		multByType[rule.label] = rule.multiplier
		totalLOC += loc
	}

	if totalLOC == 0 {
		b := newCounter()
		b.add("std", 100)
		return changeTypeInfo{1.0, b}
	}

	weightedSum := 0.0
	breakdown := newCounter()
	for _, label := range locByType.keys {
		loc := locByType.vals[label]
		weightedSum += float64(multByType[label] * float64(loc))
		breakdown.add(label, int(math.Round(float64(loc)*100.0/float64(totalLOC))))
	}
	return changeTypeInfo{weightedSum / float64(totalLOC), breakdown}
}

type complexityInfo struct {
	blendedMultiplier float64
	byLang            *counter
}

// Complexity is LOC-weighted across patches (not patch-count-weighted)
func getComplexityInfo(patches []filePatchStat) complexityInfo {
	weightedSum := 0.0
	totalLOC := 0
	byLang := newCounter()

	for _, p := range patches {
		loc := p.additions + p.deletions
		if loc == 0 {
			continue
		}
		ext := extname(p.path)
		lang := ext
		if ext == "" {
			lang = "(none)"
		}
		byLang.add(lang, loc)
		mult, ok := complexityMap[ext]
		if !ok {
			mult = 1.0
		}
		weightedSum += float64(mult * float64(loc))
		totalLOC += loc
	}

	blended := 1.0
	if totalLOC > 0 {
		blended = weightedSum / float64(totalLOC)
	}
	return complexityInfo{blended, byLang}
}

// commitPatchStats diffs a commit against its parent without rename
// detection, matching libgit2's default tree-to-tree diff.
func commitPatchStats(parent, commit *object.Commit) ([]filePatchStat, error) {
	from, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	to, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTreeWithOptions(context.Background(), from, to, nil)
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
	loc        int
	humanCost  float64
	aiCost     float64
	merges     int
	commits    int
	firstDate  time.Time
	lastDate   time.Time
	committers map[string]struct{}
}

func analyzeRepo(w io.Writer, path string, reg region, sen seniority, detail bool) error {
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

	stats := repoStats{committers: map[string]struct{}{}}

	hourlyRate := reg.rate * sen.rateMult
	locPerHour := baseLOCPerHour * sen.locMult

	if detail {
		abs, _ := filepath.Abs(path)
		fmt.Fprintf(w, "\n%s%sAnalyzing: %s%s\n", bold, cyan, abs, reset)
		fmt.Fprintf(w, "%sRegion:   %s%s%s%s%s · %s ($%.0f/hr, %.1f LOC/hr base)%s\n",
			dim, reset, white, reg.label, reset, dim, sen.label, math.Round(hourlyRate), round1(locPerHour), reset)
		fmt.Fprintln(w, dim+fmt.Sprintf("%-12s | %-11s | %-9s | %-15s | %-13s", "Date", "LOC", "Mult", "HC", "AIC")+reset)
		fmt.Fprintln(w, dim+strings.Repeat("-", 70)+reset)
	}

	err = iter.ForEach(func(commit *object.Commit) error {
		if commit.NumParents() > 1 {
			stats.merges++
			return nil
		}
		if commit.NumParents() == 0 {
			return nil
		}
		parent, err := commit.Parent(0)
		if err != nil {
			return err
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
		delta := float64(additions) + float64(float64(deletions)*0.1)
		if delta == 0 {
			return nil
		}

		langInfo := getComplexityInfo(patches)
		typeInfo := getChangeTypeInfo(patches)

		langMult := langInfo.blendedMultiplier
		typeMult := typeInfo.blendedMultiplier

		// Human cost: base rate adjusted for language complexity and change type
		adjustedLOCPerHour := locPerHour / langMult
		hCost := (delta / adjustedLOCPerHour) * hourlyRate * typeMult

		// AI cost: base API cost + complexity-scaled human review, reduced by change type
		adjOversightMins := aiHumanOversightMins * langMult * typeMult
		aCost := aiBaseCostPerCommit + float64((adjOversightMins/60.0)*hourlyRate)

		when := commit.Committer.When
		stats.loc += additions
		stats.humanCost += hCost
		stats.aiCost += aCost
		stats.commits++
		if stats.lastDate.IsZero() || when.After(stats.lastDate) {
			stats.lastDate = when
		}
		if stats.firstDate.IsZero() || when.Before(stats.firstDate) {
			stats.firstDate = when
		}
		stats.committers[strings.ToLower(commit.Author.Email)] = struct{}{}

		if detail {
			fmt.Fprintf(w, "%s%-12s%s • %sLOC:%-7s%s | %sMult:%-4.1f%s | %sHC:$%-11s%s | %sAIC:$%-9s%s\n",
				dim, when.Format("2006-01-02"), reset,
				yellow, fmtInt(delta), reset,
				complexityColor(langMult), langMult, reset,
				red, fmtMoney(hCost), reset,
				green, fmtMoney(aCost), reset)

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

	renderSummary(w, stats, reg, sen, hourlyRate)
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

func teamLabel(n int) string {
	if n == 1 {
		return "Solo developer"
	}
	return fmt.Sprintf("%d-person team", n)
}

func renderSummary(w io.Writer, s repoStats, reg region, sen seniority, hourlyRate float64) {
	savings := 0.0
	if s.humanCost > 0 {
		savings = ((s.humanCost - s.aiCost) / s.humanCost) * 100
	}
	savingsCol := green
	if savings < 0 {
		savingsCol = red
	}

	p := func(line string) { fmt.Fprintln(w, line) }

	p("")
	p(tableTop)
	p(tableHeader("FINAL ROI ANALYSIS", fmt.Sprintf("%s · %s ($%.0f/hr)", reg.label, sen.label, math.Round(hourlyRate))))
	p(tableDivider)
	p(tableRow("Commits", fmt.Sprintf("%s  (+%s merges skipped)", fmtInt(float64(s.commits)), fmtInt(float64(s.merges))), white))
	p(tableRow("LOC Added", fmtInt(float64(s.loc)), yellow))
	p(tableRow("Human Investment", "$"+fmtMoney(s.humanCost), red))
	p(tableRow("AI Agent Cost", "$"+fmtMoney(s.aiCost), green))
	p(tableRow("Net AI Savings", fmt.Sprintf("%.1f%%", round1(savings)), savingsCol))

	if s.commits > 0 {
		actualDays := math.Max(s.lastDate.Sub(s.firstDate).Seconds()/86400.0, 1)
		humanHours := s.humanCost / hourlyRate
		workingDays := humanHours / 8.0
		teamSize := max(len(s.committers), 1)
		teamDays := (workingDays / float64(teamSize)) * (7.0 / 5)
		speedMult := teamDays / actualDays

		p(tableDivider)
		p(tableHeader("BUILD SPEED", s.firstDate.Format("2006-01-02")+" → "+s.lastDate.Format("2006-01-02")))
		p(tableDivider)
		p(tableRow("AI build window", formatDuration(actualDays), white))
		p(tableRow("Human equiv", fmt.Sprintf("%s hrs / %s working days", fmtInt(math.Round(humanHours)), fmtInt(math.Round(workingDays))), white))
		committers := "committers"
		if teamSize == 1 {
			committers = "committer"
		}
		p(tableRow("Team size", fmt.Sprintf("%d %s", teamSize, committers), white))
		p(tableRow(teamLabel(teamSize), formatDuration(teamDays), red))

		var referenceSizes []int
		for _, n := range []int{1, 3, 5, 10} {
			if n != teamSize {
				referenceSizes = append(referenceSizes, n)
			}
		}
		if len(referenceSizes) > 0 {
			p(tableDivider)
			p(tableHeader("For reference", ""))
			p(tableDivider)
			for _, devs := range referenceSizes {
				calendarDays := (workingDays / float64(devs)) * (7.0 / 5)
				p(tableRow(teamLabel(devs), formatDuration(calendarDays), dim))
			}
		}

		p(tableDivider)
		p(tableRow("AI speed vs team", fmt.Sprintf("%.1fx faster", round1(speedMult)), green))
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
    --version    Print version and exit
`

func main() {
	var flags []string
	var repoPath string
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "--") {
			flags = append(flags, a)
		} else if repoPath == "" {
			repoPath = a
		}
	}

	reg, sen, detail := regions["--us"], seniorities["--senior"], false
	regionSet, senioritySet := false, false
	for _, f := range flags {
		switch {
		case f == "--version":
			fmt.Println("human-vs-ai", version)
			return
		case f == "--help":
			fmt.Print(usage)
			return
		case f == "--detail":
			detail = true
		}
		// First matching flag wins.
		if r, ok := regions[f]; ok && !regionSet {
			reg, regionSet = r, true
		}
		if s, ok := seniorities[f]; ok && !senioritySet {
			sen, senioritySet = s, true
		}
	}

	if repoPath == "" || repoPath == "-h" {
		fmt.Print(usage)
		return
	}

	if err := analyzeRepo(os.Stdout, repoPath, reg, sen, detail); err != nil {
		fmt.Printf("Error: %s.\n", strings.TrimSuffix(err.Error(), "."))
		os.Exit(1)
	}
}
