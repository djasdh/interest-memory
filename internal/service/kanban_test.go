package service

import "testing"

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
	excludes := []string{"", "   ", "\t"}
	if KanbanBoardExcluded(excludes, "default", "") {
		t.Error("blank exclude entries must be ignored")
	}
}
