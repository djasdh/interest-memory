package service

import (
	"testing"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/store"
)

func TestKanbanBoardExcludedEmptyConfig(t *testing.T) {
	// 验收 3：未配置排除项（nil 或空数组）时行为与之前完全一致——永不排除。
	if KanbanBoardExcluded(nil, "default", "Default") {
		t.Error("nil exclude list excluded a board")
	}
	if KanbanBoardExcluded([]string{}, "default", "Default") {
		t.Error("empty exclude list excluded a board")
	}
}

func TestKanbanBoardExcludedMatchesID(t *testing.T) {
	// 验收 1：按 board ID（slug）匹配。
	excludes := []string{"default", "beta"}
	if !KanbanBoardExcluded(excludes, "default", "Default") {
		t.Error("board ID 'default' should match")
	}
	if !KanbanBoardExcluded(excludes, "beta", "") {
		t.Error("board ID 'beta' should match")
	}
	if KanbanBoardExcluded(excludes, "gamma", "Gamma") {
		t.Error("board ID 'gamma' should not match")
	}
}

func TestKanbanBoardExcludedMatchesName(t *testing.T) {
	// 验收 1：按显示名称匹配。
	excludes := []string{"My Project Board"}
	if !KanbanBoardExcluded(excludes, "my-project-board", "My Project Board") {
		t.Error("board name should match")
	}
}

func TestKanbanBoardExcludedCaseInsensitive(t *testing.T) {
	excludes := []string{"DEFAULT"}
	if !KanbanBoardExcluded(excludes, "default", "") {
		t.Error("ID match must be case-insensitive")
	}
	if !KanbanBoardExcluded(excludes, "", "default") {
		t.Error("name match must be case-insensitive")
	}
}

func TestKanbanBoardExcludedWhitespaceTrimmed(t *testing.T) {
	excludes := []string{"  default  ", "\tbeta\n"}
	if !KanbanBoardExcluded(excludes, "default", "") {
		t.Error("exclude entries must be whitespace-trimmed")
	}
	if !KanbanBoardExcluded(excludes, "", "beta") {
		t.Error("exclude entries must be whitespace-trimmed (name side)")
	}
	if KanbanBoardExcluded(excludes, "", "") {
		t.Error("empty board identity must never match")
	}
}

func TestKanbanBoardExcludedIgnoresBlankEntries(t *testing.T) {
	excludes := []string{"", "   ", "	"}
	if KanbanBoardExcluded(excludes, "default", "") {
		t.Error("blank exclude entries must be ignored")
	}
}

func TestKanbanBoardExcludedPartialList(t *testing.T) {
	// 部分排除：列表中命中的看板被排除，未列出的不受影响；
	// 同一列表条目可分别命中 ID 或显示名称。
	excludes := []string{"alpha", "beta"}
	cases := []struct {
		id, name string
		want     bool
	}{
		{"alpha", "Alpha", true},
		{"beta", "Beta", true},
		{"gamma", "Gamma", false},
		{"", "Gamma", false},
		{"", "beta", true}, // 名称命中
	}
	for _, c := range cases {
		if got := KanbanBoardExcluded(excludes, c.id, c.name); got != c.want {
			t.Errorf("KanbanBoardExcluded(%v, %q, %q) = %v, want %v", excludes, c.id, c.name, got, c.want)
		}
	}
}

func TestKanbanBoardExcludedExcludesAllListed(t *testing.T) {
	// 排除所有看板：列表覆盖全部 board 身份时每个都命中。
	excludes := []string{"default", "beta", "My Project Board"}
	boards := []struct{ id, name string }{
		{"default", "Default"},
		{"", "beta"},
		{"my-project-board", "My Project Board"},
	}
	for _, b := range boards {
		if !KanbanBoardExcluded(excludes, b.id, b.name) {
			t.Errorf("board %q/%q should be excluded by %v", b.id, b.name, excludes)
		}
	}
	// 回归保护：空列表不构成"排除所有"，行为与未配置一致。
	if KanbanBoardExcluded([]string{}, "default", "") {
		t.Error("empty list must not exclude every board")
	}
}

func TestKanbanBoardExcludedBoardIdentityTrimmed(t *testing.T) {
	// 匹配规则：board 身份（ID 与名称）两侧同样做空白 trim。
	excludes := []string{"default"}
	if !KanbanBoardExcluded(excludes, "  default  ", "") {
		t.Error("board ID with surrounding whitespace should match")
	}
	if !KanbanBoardExcluded(excludes, "", "	default\n") {
		t.Error("board name with surrounding whitespace should match")
	}
}

func TestKanbanBoardExcludedEitherIdentityMatches(t *testing.T) {
	// 匹配规则：ID 或名称任一命中即排除——名称匹配但 ID 不同，或反之。
	excludes := []string{"Default"}
	if !KanbanBoardExcluded(excludes, "another-slug", "Default") {
		t.Error("name match must exclude even when ID differs")
	}
	if !KanbanBoardExcluded(excludes, "default", "Other Name") {
		t.Error("ID match must exclude even when name differs")
	}
	if KanbanBoardExcluded(excludes, "another-slug", "Other Name") {
		t.Error("neither identity matching must not exclude")
	}
}

func TestServiceKanbanBoardExcludedReadsConfig(t *testing.T) {
	// 方法级闭环：KanbanBoardExcluded 方法读的是配置里的 kanban_exclude，
	// 配置驱动匹配——nil/空/有值三种形态。
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	svc := &Service{cfg: config.Default(), store: st}
	if svc.KanbanBoardExcluded("default", "Default") {
		t.Error("unconfigured service must not exclude any board")
	}

	svc.cfg.InterestMemory.KanbanExclude = []string{"default", "board-x"}
	if !svc.KanbanBoardExcluded("default", "Default") {
		t.Error("configured service must exclude 'default'")
	}
	if !svc.KanbanBoardExcluded("board-x", "") {
		t.Error("configured service must exclude 'board-x' by ID")
	}
	if svc.KanbanBoardExcluded("keep-me", "Keep Me") {
		t.Error("configured service must keep non-listed boards")
	}
}
