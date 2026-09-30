package main

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestLineDiffStat(t *testing.T) {
	tests := []struct {
		name       string
		a, b       string
		adds, dels int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"new file", "", "a\nb\nc\n", 3, 0},
		{"deleted file", "a\nb\n", "", 0, 2},
		{"append", "a\n", "a\nb\n", 1, 0},
		{"modify middle", "a\nb\nc\n", "a\nX\nc\n", 1, 1},
		{"missing final newline", "a\nb", "a\nb\n", 1, 1},
		{"swap", "a\nb\n", "b\na\n", 1, 1},
		{"repeated lines", "x\nx\nx\ny\n", "x\ny\nx\nx\n", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adds, dels := lineDiffStat([]byte(tt.a), []byte(tt.b))
			if adds != tt.adds || dels != tt.dels {
				t.Errorf("got +%d -%d, want +%d -%d", adds, dels, tt.adds, tt.dels)
			}
		})
	}
}

func TestEditDistanceBoundedWork(t *testing.T) {
	// Interleaved inputs with no long common runs still terminate with a
	// sound answer.
	var a, b strings.Builder
	for i := range 2000 {
		a.WriteString(strings.Repeat("a", i%3) + "\n")
		b.WriteString(strings.Repeat("a", (i+1)%3) + "\n")
	}
	adds, dels := lineDiffStat([]byte(a.String()), []byte(b.String()))
	if adds != dels || adds > 2000 {
		t.Errorf("got +%d -%d", adds, dels)
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("hello\nworld\n")) {
		t.Error("text reported as binary")
	}
	if !isBinary([]byte("PNG\x00\x01")) {
		t.Error("NUL byte not reported as binary")
	}
	late := append([]byte(strings.Repeat("a", binaryCheckBytes)), 0)
	if isBinary(late) {
		t.Error("NUL past the check window reported as binary")
	}
}

func TestChangeTypeFor(t *testing.T) {
	tests := map[string]string{
		"dist/app.js":              "generated",
		"assets/app.min.js":        "generated",
		"images/logo.svg":          "std",
		"packages/app/dist/app.js": "generated",
		"Gemfile.lock":             "dep-update",
		"web/package.json":         "dep-update",
		"go.sum":                   "dep-update",
		"spec/models/user_spec.rb": "test",
		"src/app.test.ts":          "test",
		"tests/helpers.py":         "test",
		"README.md":                "docs",
		"config/app.yml":           "config",
		"infra/main.tf":            "config",
		"src/main.go":              "std",
		"Makefile":                 "std",
	}
	for path, want := range tests {
		if got := changeTypeFor(path).label; got != want {
			t.Errorf("changeTypeFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestExtname(t *testing.T) {
	tests := map[string]string{
		"a/b/main.go":    ".go",
		"archive.tar.gz": ".gz",
		".bashrc":        "",
		"dir/.env.local": ".local",
		"Makefile":       "",
	}
	for path, want := range tests {
		if got := extname(path); got != want {
			t.Errorf("extname(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestBlendedMultipliers(t *testing.T) {
	patches := []filePatchStat{
		{path: "main.rs", additions: 200},
		{path: "README.md", additions: 5},
		{path: "logo.png"}, // binary: no lines, ignored
	}
	lang := getComplexityInfo(patches, 0.1)
	want := (200*2.2 + 5*0.3) / 205
	if diff := lang.blendedMultiplier - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("lang mult = %v, want %v", lang.blendedMultiplier, want)
	}
	if got := lang.byLang.keys; len(got) != 2 {
		t.Errorf("byLang keys = %v", got)
	}

	types := getChangeTypeInfo(patches, 0.1)
	if types.breakdown.vals["std"] != 98 || types.breakdown.vals["docs"] != 2 {
		t.Errorf("breakdown = %v", types.breakdown.vals)
	}

	empty := getChangeTypeInfo(nil, 0.1)
	if empty.blendedMultiplier != 1.0 || empty.breakdown.vals["std"] != 100 {
		t.Errorf("empty = %+v", empty)
	}
}

func TestEstimateCommitCalculatesPerFile(t *testing.T) {
	cfg := estimationProfiles["base"]
	patches := []filePatchStat{
		{path: "main.rs", additions: 100},
		{path: "README.md", deletions: 1000},
	}
	got := estimateCommit(patches, 100, seniorities["--senior"], cfg)
	// Rust: 100*2.2. Markdown deletion: 1000*0.1*0.3. Docs are
	// classified for display but receive no second effort discount.
	wantUnits := 250.0
	if math.Abs(got.effortUnits-wantUnits) > 1e-9 {
		t.Fatalf("effort units = %v, want %v", got.effortUnits, wantUnits)
	}
	if math.Abs(got.humanCost-1000) > 1e-9 {
		t.Errorf("human cost = %v, want 1000", got.humanCost)
	}
}

func TestHumanEffortIsInvariantToCommitSplitting(t *testing.T) {
	cfg := estimationProfiles["base"]
	sen := seniorities["--senior"]
	combined := estimateCommit([]filePatchStat{
		{path: "main.rs", additions: 100},
		{path: "README.md", additions: 100},
	}, 100, sen, cfg)
	rust := estimateCommit([]filePatchStat{{path: "main.rs", additions: 100}}, 100, sen, cfg)
	docs := estimateCommit([]filePatchStat{{path: "README.md", additions: 100}}, 100, sen, cfg)
	if math.Abs(combined.humanCost-rust.humanCost-docs.humanCost) > 1e-9 {
		t.Errorf("combined cost %v != split cost %v", combined.humanCost, rust.humanCost+docs.humanCost)
	}
}

func TestChangeTypePercentagesTotal100(t *testing.T) {
	info := getChangeTypeInfo([]filePatchStat{
		{path: "main.go", additions: 1},
		{path: "README.md", additions: 1},
		{path: "config.yml", additions: 1},
	}, 0.1)
	total := 0
	for _, value := range info.breakdown.vals {
		total += value
	}
	if total != 100 {
		t.Errorf("percentage total = %d, want 100", total)
	}
}

func TestRootCommitIsDiffedAgainstEmptyTree(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/main.go", []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("main.go"); err != nil {
		t.Fatal(err)
	}
	hash, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@example.com", When: time.Unix(1, 0),
	}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	patches, err := commitPatchStats(nil, commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0].additions != 3 || patches[0].deletions != 0 {
		t.Fatalf("root patches = %+v", patches)
	}
}

func TestRenameDoesNotCountAsRewrittenContent(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/old.go", []byte("package renamed\n\nfunc Example() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	_, _ = wt.Add("old.go")
	when := time.Unix(1, 0)
	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: when}
	parentHash, err := wt.Commit("initial", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Move("old.go", "new.go"); err != nil {
		t.Fatal(err)
	}
	sig.When = time.Unix(2, 0)
	commitHash, err := wt.Commit("rename", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := repo.CommitObject(parentHash)
	commit, _ := repo.CommitObject(commitHash)
	patches, err := commitPatchStats(parent, commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0].additions != 0 || patches[0].deletions != 0 {
		t.Fatalf("rename patches = %+v", patches)
	}
}

func TestAITokenEstimate(t *testing.T) {
	tokens, cost := estimateAITokens(100, defaultAITokenModel)
	if tokens != 250_000 {
		t.Errorf("tokens = %v, want 250000", tokens)
	}
	if math.Abs(cost-0.675) > 1e-9 {
		t.Errorf("cost = %v, want 0.675", cost)
	}
}

func TestFormatting(t *testing.T) {
	cases := []struct{ got, want string }{
		{fmtInt(0), "0"},
		{fmtInt(999.9), "999"},
		{fmtInt(16693), "16,693"},
		{fmtInt(1234567), "1,234,567"},
		{fmtMoney(0.185), "0.18"},
		{fmtMoney(79067.781), "79,067.78"},
		{fmtMoney(-1234.5), "-1,234.50"},
		{formatDuration(0.5), "less than a day"},
		{formatDuration(20), "20.0 days"},
		{formatDuration(168.25), "5.6 months"},
		{formatDuration(830.9), "2.3 years"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
