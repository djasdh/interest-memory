package service

import "strings"

// KanbanBoardExcluded reports whether a kanban board — identified by its
// slug/ID (e.g. "default") and optional display name — matches any entry in
// the exclude list. Matching is case-insensitive and whitespace-trimmed,
// applied against the board ID and the board name. An empty exclude list
// excludes nothing (pre-configuration behaviour). A board with neither an ID
// nor a name never matches.
func KanbanBoardExcluded(excludes []string, boardID, boardName string) bool {
	if len(excludes) == 0 {
		return false
	}
	id := strings.TrimSpace(boardID)
	name := strings.TrimSpace(boardName)
	if id == "" && name == "" {
		return false
	}
	for _, e := range excludes {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if id != "" && strings.EqualFold(e, id) {
			return true
		}
		if name != "" && strings.EqualFold(e, name) {
			return true
		}
	}
	return false
}

// KanbanBoardExcluded is the Service-level wrapper: it consults the
// configured interestmemory.kanban_exclude list. Used by the HTTP layer to
// drop excluded boards before any storage / embedding / token accounting.
func (s *Service) KanbanBoardExcluded(boardID, boardName string) bool {
	return KanbanBoardExcluded(s.cfg.InterestMemory.KanbanExclude, boardID, boardName)
}
