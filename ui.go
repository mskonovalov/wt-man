package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

type screen int

const (
	browseScreen screen = iota
	reviewScreen
	deletingScreen
	resultsScreen
)

type sessionMode int

const (
	allSessions sessionMode = iota
	withUnarchivedSessions
	withoutUnarchivedSessions
)

type row struct {
	repository int
	worktree   int
}

type deletionResult struct {
	Path          string
	Branch        string
	Missing       bool
	Removed       bool
	BranchDeleted bool
	Err           error
}

type deletionFinishedMsg []deletionResult

type modificationTimeMsg struct {
	row        row
	modifiedAt time.Time
}

type model struct {
	repositories      []repository
	rows              []row
	visible           []row
	selected          map[string]bool
	cursor            int
	offset            int
	width             int
	height            int
	query             string
	filtering         bool
	screen            screen
	deleteBranches    bool
	results           []deletionResult
	repositoryWidth   int
	sessionMode       sessionMode
	modificationQueue []row
}

func newModel(repositories []repository) model {
	cache := readModificationCache()
	cacheCutoff := time.Now().Add(-modificationCacheTTL)
	m := model{
		repositories: repositories,
		selected:     make(map[string]bool),
		height:       24,
		width:        100,
	}
	for repositoryIndex, repo := range repositories {
		if width := utf8.RuneCountInString(repo.Name); width > m.repositoryWidth {
			m.repositoryWidth = width
		}
		for worktreeIndex := range repo.Worktrees {
			current := row{repository: repositoryIndex, worktree: worktreeIndex}
			m.rows = append(m.rows, current)
			if !repo.Worktrees[worktreeIndex].Missing {
				entry := cache[repo.Worktrees[worktreeIndex].Path]
				if entry.ScannedAt.After(cacheCutoff) {
					m.repositories[repositoryIndex].Worktrees[worktreeIndex].ModifiedAt = entry.ModifiedAt
				} else {
					m.modificationQueue = append(m.modificationQueue, current)
				}
			}
		}
	}
	m.visible = append([]row(nil), m.rows...)
	return m
}

func (m model) Init() tea.Cmd {
	if len(m.modificationQueue) == 0 {
		return nil
	}
	current := m.modificationQueue[0]
	return scanModificationTime(current, m.item(current).Path)
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.ensureCursorVisible()
		return m, nil
	case deletionFinishedMsg:
		m.results = []deletionResult(message)
		m.screen = resultsScreen
		return m, nil
	case modificationTimeMsg:
		m.repositories[message.row.repository].Worktrees[message.row.worktree].ModifiedAt = message.modifiedAt
		m.modificationQueue = m.modificationQueue[1:]
		if len(m.modificationQueue) == 0 {
			return m, nil
		}
		current := m.modificationQueue[0]
		return m, scanModificationTime(current, m.item(current).Path)
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m.updateKey(message.String())
	}
	return m, nil
}

func (m model) updateKey(key string) (tea.Model, tea.Cmd) {
	if m.screen == deletingScreen {
		return m, nil
	}
	if m.screen == resultsScreen {
		if key == "q" || key == "enter" || key == "esc" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.screen == reviewScreen {
		switch key {
		case "q":
			return m, tea.Quit
		case "b", "esc":
			m.screen = browseScreen
		case "x":
			m.deleteBranches = !m.deleteBranches
		case "D":
			m.screen = deletingScreen
			return m, deleteSelected(m.repositories, m.selectedRows(), m.deleteBranches)
		}
		return m, nil
	}

	if m.filtering {
		switch key {
		case "enter":
			m.filtering = false
		case "esc":
			m.filtering = false
			m.query = ""
			m.applyFilter()
		case "backspace", "ctrl+h":
			if m.query != "" {
				_, size := utf8.DecodeLastRuneInString(m.query)
				m.query = m.query[:len(m.query)-size]
				m.applyFilter()
			}
		default:
			if len([]rune(key)) == 1 {
				m.query += key
				m.applyFilter()
			}
		}
		return m, nil
	}

	switch key {
	case "q":
		return m, tea.Quit
	case "/":
		m.filtering = true
	case "u":
		m.sessionMode = (m.sessionMode + 1) % 3
		m.applyFilter()
	case "r":
		if len(m.visible) > 0 {
			current := m.visible[m.cursor]
			return m.queueModificationRefresh([]row{current})
		}
	case "R":
		return m.queueModificationRefresh(m.rows)
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.visible)-1 {
			m.cursor++
		}
	case "pgup":
		m.cursor -= m.pageSize()
		if m.cursor < 0 {
			m.cursor = 0
		}
	case "pgdown":
		m.cursor += m.pageSize()
		if m.cursor >= len(m.visible) {
			m.cursor = len(m.visible) - 1
		}
	case "space":
		if len(m.visible) > 0 {
			path := m.item(m.visible[m.cursor]).Path
			m.selected[path] = !m.selected[path]
		}
	case "a":
		m.toggleAllVisible()
	case "enter":
		if len(m.selectedRows()) > 0 {
			m.screen = reviewScreen
		}
	}
	m.ensureCursorVisible()
	return m, nil
}

func (m *model) applyFilter() {
	query := strings.ToLower(m.query)
	m.visible = m.visible[:0]
	for _, current := range m.rows {
		repo := m.repositories[current.repository]
		item := repo.Worktrees[current.worktree]
		haystack := strings.ToLower(repo.Name + " " + item.Branch + " " + item.Path)
		hasUnarchived := item.Sessions.Claude+item.Sessions.Codex > 0
		sessionMatches := m.sessionMode == allSessions ||
			(m.sessionMode == withUnarchivedSessions && hasUnarchived) ||
			(m.sessionMode == withoutUnarchivedSessions && !hasUnarchived)
		if strings.Contains(haystack, query) && sessionMatches {
			m.visible = append(m.visible, current)
		}
	}
	m.cursor = 0
	m.offset = 0
}

func (m *model) toggleAllVisible() {
	allSelected := len(m.visible) > 0
	for _, current := range m.visible {
		if !m.selected[m.item(current).Path] {
			allSelected = false
			break
		}
	}
	for _, current := range m.visible {
		m.selected[m.item(current).Path] = !allSelected
	}
}

func (m model) selectedRows() []row {
	var selected []row
	for _, current := range m.rows {
		if m.selected[m.item(current).Path] {
			selected = append(selected, current)
		}
	}
	return selected
}

func (m model) item(current row) worktree {
	return m.repositories[current.repository].Worktrees[current.worktree]
}

func (m model) pageSize() int {
	if m.height < 12 {
		return 1
	}
	return m.height - 11
}

func (m *model) ensureCursorVisible() {
	if len(m.visible) == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.pageSize() {
		m.offset = m.cursor - m.pageSize() + 1
	}
}

func (m model) queueModificationRefresh(rows []row) (tea.Model, tea.Cmd) {
	wasIdle := len(m.modificationQueue) == 0
	queued := make(map[row]bool, len(m.modificationQueue))
	for _, current := range m.modificationQueue {
		queued[current] = true
	}
	for _, current := range rows {
		item := m.item(current)
		if item.Missing || queued[current] {
			continue
		}
		m.repositories[current.repository].Worktrees[current.worktree].ModifiedAt = time.Time{}
		m.modificationQueue = append(m.modificationQueue, current)
		queued[current] = true
	}
	if !wasIdle || len(m.modificationQueue) == 0 {
		return m, nil
	}
	current := m.modificationQueue[0]
	return m, scanModificationTime(current, m.item(current).Path)
}

func (m model) View() tea.View {
	var content string
	switch m.screen {
	case browseScreen:
		content = m.browseView()
	case reviewScreen:
		content = m.reviewView()
	case deletingScreen:
		content = "\nDeleting selected worktrees…\n"
	case resultsScreen:
		content = m.resultsView()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m model) browseView() string {
	var output strings.Builder
	fmt.Fprintf(&output, "\n\x1b[1mwt-man\x1b[0m  %d worktrees  %d selected  sessions: %s\n",
		len(m.visible), len(m.selectedRows()), m.sessionMode.label())
	if m.filtering {
		fmt.Fprintf(&output, "Filter: %s█\n\n", m.query)
	} else if m.query != "" {
		fmt.Fprintf(&output, "Filter: %s  (/ to edit)\n\n", m.query)
	} else {
		output.WriteString("/ filter  u sessions  r refresh  R refresh all  space select  a all  enter review  q quit\n\n")
	}

	end := m.offset + m.pageSize()
	if end > len(m.visible) {
		end = len(m.visible)
	}
	fmt.Fprintf(&output, "      %-*s %-10s %-10s %-8s %-16s %s\n",
		m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SESSIONS", "BRANCH", "PATH")
	for index := m.offset; index < end; index++ {
		current := m.visible[index]
		repo := m.repositories[current.repository]
		item := repo.Worktrees[current.worktree]
		pointer := " "
		if index == m.cursor {
			pointer = "›"
		}
		checked := " "
		if m.selected[item.Path] {
			checked = "x"
		}
		repoName := repo.Name
		if index > m.offset && m.visible[index-1].repository == current.repository {
			repoName = ""
		}
		created := "unknown"
		if item.Missing {
			created = "missing"
		} else if !item.CreatedAt.IsZero() {
			created = item.CreatedAt.Format("2006-01-02")
		}
		modified := "scanning"
		if item.Missing {
			modified = "missing"
		} else if !item.ModifiedAt.IsZero() {
			modified = item.ModifiedAt.Format("2006-01-02")
		} else if len(m.modificationQueue) == 0 {
			modified = "unknown"
		}
		branch := item.Branch
		if branch == "" {
			branch = "detached"
		}
		warning := ""
		if item.Sessions.Claude+item.Sessions.Codex > 0 {
			warning = " !"
		}
		sessions := fmt.Sprintf("C%d X%d%s", item.Sessions.Claude, item.Sessions.Codex, warning)
		line := fmt.Sprintf("%s [%s] %-*s %-10s %-10s %-8s %-16s %s",
			pointer, checked, m.repositoryWidth, repoName, created, modified, sessions,
			truncate(branch, 16), item.Path)
		output.WriteString(truncate(line, m.width))
		output.WriteByte('\n')
	}
	if len(m.visible) == 0 {
		output.WriteString("No matching worktrees.\n")
	} else {
		item := m.item(m.visible[m.cursor])
		output.WriteByte('\n')
		output.WriteString(truncate("Path: "+item.Path, m.width))
		output.WriteByte('\n')
		branchDetails := "Branch: " + item.Branch
		if item.Missing {
			branchDetails += "  State: missing (prunable; Git record only)"
		}
		output.WriteString(truncate(branchDetails, m.width))
		output.WriteByte('\n')
	}
	return output.String()
}

func scanModificationTime(current row, path string) tea.Cmd {
	return func() tea.Msg {
		modifiedAt := modificationTime(path)
		writeModificationCacheEntry(path, modifiedAt)
		return modificationTimeMsg{row: current, modifiedAt: modifiedAt}
	}
}

func (mode sessionMode) label() string {
	switch mode {
	case withUnarchivedSessions:
		return "with unarchived"
	case withoutUnarchivedSessions:
		return "without unarchived"
	default:
		return "all"
	}
}

func (m model) reviewView() string {
	selected := m.selectedRows()
	var output strings.Builder
	fmt.Fprintf(&output, "\n\x1b[1mReview %d worktrees\x1b[0m\n\n", len(selected))
	limit := m.height - 8
	if limit < 1 {
		limit = 1
	}
	for index, current := range selected {
		if index >= limit {
			fmt.Fprintf(&output, "  … and %d more\n", len(selected)-index)
			break
		}
		repo := m.repositories[current.repository]
		item := repo.Worktrees[current.worktree]
		fmt.Fprintf(&output, "  [%s] %s", repo.Name, item.Path)
		if item.Missing {
			output.WriteString("  \x1b[33mmissing: delete Git record only\x1b[0m")
		} else {
			output.WriteString("  delete files and Git record")
		}
		if item.Sessions.Claude+item.Sessions.Codex > 0 {
			fmt.Fprintf(&output, "  \x1b[33mClaude %d, Codex %d unarchived\x1b[0m", item.Sessions.Claude, item.Sessions.Codex)
		}
		output.WriteByte('\n')
	}
	branches := "keep local branches"
	if m.deleteBranches {
		branches = "DELETE local branches"
	}
	fmt.Fprintf(&output, "\n[x] %s\n[b] back  [D] delete permanently  [q] quit\n", branches)
	return output.String()
}

func (m model) resultsView() string {
	var output strings.Builder
	output.WriteString("\n\x1b[1mDeletion results\x1b[0m\n\n")
	for _, result := range m.results {
		if result.Err != nil {
			fmt.Fprintf(&output, "✗ %s: %v\n", result.Path, result.Err)
			continue
		}
		fmt.Fprintf(&output, "✓ %s", result.Path)
		if result.Missing {
			output.WriteString(" (deleted stale Git record)")
		} else {
			output.WriteString(" (deleted files and Git record)")
		}
		if result.BranchDeleted {
			fmt.Fprintf(&output, " (deleted branch %s)", result.Branch)
		}
		output.WriteByte('\n')
	}
	output.WriteString("\nPress enter or q to exit.\n")
	return output.String()
}

func deleteSelected(repositories []repository, selected []row, deleteBranches bool) tea.Cmd {
	return func() tea.Msg {
		results := make([]deletionResult, 0, len(selected))
		for _, current := range selected {
			repo := repositories[current.repository]
			item := repo.Worktrees[current.worktree]
			results = append(results, removeWorktree(context.Background(), repo, item, deleteBranches))
		}
		sort.SliceStable(results, func(i, j int) bool { return results[i].Path < results[j].Path })
		return deletionFinishedMsg(results)
	}
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}
