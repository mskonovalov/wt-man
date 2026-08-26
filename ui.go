package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type screen int

const (
	browseScreen screen = iota
	reviewScreen
	deletingScreen
	resultsScreen
)

type safetyMode int

const (
	allSafetyStatuses safetyMode = iota
	safeOnly
	notSafeOnly
	checkingOnly
)

type safetyState int

const (
	safetyChecking safetyState = iota
	safetySafe
	safetyNotSafe
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

type deletionProgressMsg struct {
	result deletionResult
}

type modificationTimeMsg struct {
	generation int
	row        row
	modifiedAt time.Time
}

type changesStatusMsg struct {
	generation int
	row        row
	hasChanges bool
	known      bool
}

type mergeStatusMsg struct {
	generation int
	repository int
	merged     map[string]bool
	target     string
}

type githubMergeStatusMsg struct {
	generation    int
	authenticated bool
	merged        []row
}

type model struct {
	repositories        []repository
	rows                []row
	visible             []row
	selected            map[string]bool
	cursor              int
	offset              int
	width               int
	height              int
	query               string
	filtering           bool
	screen              screen
	deleteBranches      bool
	results             []deletionResult
	repositoryWidth     int
	branchWidth         int
	safetyMode          safetyMode
	modificationQueue   []row
	modificationTotal   int
	modificationDone    int
	changesQueue        []row
	changesTotal        int
	changesDone         int
	mergeQueue          []int
	mergeTotal          int
	mergeDone           int
	githubMergePending  bool
	githubAuthChecked   bool
	githubAuthAvailable bool
	deletionQueue       []row
	deletionTotal       int
	deletionWaiting     bool
	generation          int
}

func newModel(repositories []repository) model {
	cache := readModificationCache()
	cacheCutoff := time.Now().Add(-modificationCacheTTL)
	m := model{
		repositories:    repositories,
		selected:        make(map[string]bool),
		height:          24,
		width:           100,
		repositoryWidth: utf8.RuneCountInString("REPOSITORY"),
		branchWidth:     utf8.RuneCountInString("BRANCH"),
	}
	for repositoryIndex, repo := range repositories {
		for _, item := range repo.Worktrees {
			if item.Branch != "" {
				m.mergeQueue = append(m.mergeQueue, repositoryIndex)
				break
			}
		}
		if width := utf8.RuneCountInString(repo.Name); width > m.repositoryWidth {
			m.repositoryWidth = width
		}
		for worktreeIndex := range repo.Worktrees {
			branch := repo.Worktrees[worktreeIndex].Branch
			if branch == "" {
				branch = "detached"
			}
			if width := utf8.RuneCountInString(branch); width > m.branchWidth {
				m.branchWidth = width
			}
			current := row{repository: repositoryIndex, worktree: worktreeIndex}
			m.rows = append(m.rows, current)
			item := &m.repositories[repositoryIndex].Worktrees[worktreeIndex]
			if item.Missing {
				item.ChangesChecked = true
				item.ChangesKnown = true
			} else {
				m.changesQueue = append(m.changesQueue, current)
			}
			if item.Branch == "" {
				item.MergeChecked = true
			}
			if !item.Missing {
				entry := cache[repo.Worktrees[worktreeIndex].Path]
				if entry.ScannedAt.After(cacheCutoff) {
					m.repositories[repositoryIndex].Worktrees[worktreeIndex].ModifiedAt = entry.ModifiedAt
				} else {
					m.modificationQueue = append(m.modificationQueue, current)
				}
			}
		}
	}
	m.modificationTotal = len(m.modificationQueue)
	m.changesTotal = len(m.changesQueue)
	m.mergeTotal = len(m.mergeQueue)
	m.githubMergePending = len(m.mergeQueue) == 0
	m.visible = append([]row(nil), m.rows...)
	return m
}

func (m model) Init() tea.Cmd {
	var commands []tea.Cmd
	if len(m.modificationQueue) > 0 {
		current := m.modificationQueue[0]
		commands = append(commands, scanModificationTime(m.generation, current, m.item(current).Path))
	}
	if len(m.changesQueue) > 0 {
		current := m.changesQueue[0]
		commands = append(commands, scanChangesStatus(m.generation, current, m.item(current).Path))
	}
	if len(m.mergeQueue) > 0 {
		commands = append(commands, scanMergeStatus(m.generation, m.mergeQueue[0], m.repositories[m.mergeQueue[0]].MainPath))
	} else if m.githubMergePending {
		commands = append(commands, m.scanGitHubMergeStatus())
	}
	return tea.Batch(commands...)
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.ensureCursorVisible()
		return m, nil
	case deletionProgressMsg:
		m.results = append(m.results, message.result)
		m.deletionQueue = m.deletionQueue[1:]
		if len(m.deletionQueue) == 0 {
			m.screen = resultsScreen
			return m, nil
		}
		return m, m.deleteNext()
	case modificationTimeMsg:
		if message.generation != m.generation {
			return m, nil
		}
		m.repositories[message.row.repository].Worktrees[message.row.worktree].ModifiedAt = message.modifiedAt
		m.modificationQueue = m.modificationQueue[1:]
		m.modificationDone++
		if m.screen == deletingScreen && m.deletionWaiting {
			m.deletionWaiting = false
			return m, m.deleteNext()
		}
		if len(m.modificationQueue) == 0 {
			return m, nil
		}
		current := m.modificationQueue[0]
		return m, scanModificationTime(m.generation, current, m.item(current).Path)
	case changesStatusMsg:
		if message.generation != m.generation {
			return m, nil
		}
		item := &m.repositories[message.row.repository].Worktrees[message.row.worktree]
		item.ChangesChecked = true
		item.ChangesKnown = message.known
		item.HasChanges = message.hasChanges
		m.changesQueue = m.changesQueue[1:]
		m.changesDone++
		if m.safetyMode != allSafetyStatuses {
			m.applyFilter()
		}
		if len(m.changesQueue) == 0 {
			return m, nil
		}
		current := m.changesQueue[0]
		return m, scanChangesStatus(m.generation, current, m.item(current).Path)
	case mergeStatusMsg:
		if message.generation != m.generation {
			return m, nil
		}
		repo := &m.repositories[message.repository]
		repo.MergeTarget = message.target
		for worktreeIndex := range repo.Worktrees {
			item := &repo.Worktrees[worktreeIndex]
			item.MergeChecked = true
			if item.Branch != "" && message.target != "" {
				item.Merged = message.merged[item.Branch]
				item.MergeKnown = true
				item.MergeSource = "Git"
			}
		}
		m.mergeQueue = m.mergeQueue[1:]
		m.mergeDone++
		if m.safetyMode != allSafetyStatuses {
			m.applyFilter()
		}
		if len(m.mergeQueue) > 0 {
			next := m.mergeQueue[0]
			return m, scanMergeStatus(m.generation, next, m.repositories[next].MainPath)
		}
		m.githubMergePending = true
		return m, m.scanGitHubMergeStatus()
	case githubMergeStatusMsg:
		if message.generation != m.generation {
			return m, nil
		}
		m.githubMergePending = false
		m.githubAuthChecked = true
		m.githubAuthAvailable = message.authenticated
		for _, current := range message.merged {
			item := &m.repositories[current.repository].Worktrees[current.worktree]
			item.MergeChecked = true
			item.Merged = true
			item.MergeKnown = true
			item.MergeSource = "GitHub"
		}
		if m.safetyMode != allSafetyStatuses {
			m.applyFilter()
		}
		return m, nil
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" {
			if m.screen == deletingScreen {
				return m, nil
			}
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
		if key == "q" {
			return m, tea.Quit
		}
		if key == "enter" || key == "esc" {
			return m.returnToList()
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
			if m.selectedSafetyChecking() {
				return m, nil
			}
			m.screen = deletingScreen
			m.results = nil
			m.deletionQueue = m.selectedRows()
			m.deletionTotal = len(m.deletionQueue)
			if len(m.modificationQueue) > 0 {
				m.deletionWaiting = true
				return m, nil
			}
			return m, m.deleteNext()
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
	case "s":
		m.safetyMode = (m.safetyMode + 1) % 4
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
		state, _ := m.safety(current)
		safetyMatches := m.safetyMode == allSafetyStatuses ||
			(m.safetyMode == safeOnly && state == safetySafe) ||
			(m.safetyMode == notSafeOnly && state == safetyNotSafe) ||
			(m.safetyMode == checkingOnly && state == safetyChecking)
		if strings.Contains(haystack, query) && safetyMatches {
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

func (m model) selectedSafetyChecking() bool {
	for _, current := range m.selectedRows() {
		state, _ := m.safety(current)
		if state == safetyChecking {
			return true
		}
	}
	return false
}

func (m model) item(current row) worktree {
	return m.repositories[current.repository].Worktrees[current.worktree]
}

func (m model) safety(current row) (safetyState, []string) {
	item := m.item(current)
	if !item.ChangesChecked || !item.MergeChecked ||
		(item.Branch != "" && !item.Merged && !m.githubAuthChecked) {
		return safetyChecking, nil
	}
	var reasons []string
	if item.Locked {
		reasons = append(reasons, "worktree is locked")
	}
	if !item.ChangesKnown {
		reasons = append(reasons, "local changes could not be checked")
	} else if item.HasChanges {
		reasons = append(reasons, "has local changes")
	}
	if !item.Sessions.ClaudeKnown || !item.Sessions.CodexKnown {
		reasons = append(reasons, "session status is unknown")
	}
	if item.Sessions.Claude+item.Sessions.Codex > 0 {
		reasons = append(reasons, "has unarchived sessions")
	}
	if !item.MergeKnown {
		reasons = append(reasons, "committed work could not be checked")
	} else if !item.Merged {
		reasons = append(reasons, "committed work is not in the default branch")
	}
	if len(reasons) > 0 {
		return safetyNotSafe, reasons
	}
	return safetySafe, nil
}

func safetyLabel(state safetyState) string {
	switch state {
	case safetySafe:
		return "yes"
	case safetyNotSafe:
		return "NO"
	default:
		return "…"
	}
}

func (m model) pageSize() int {
	if m.height < 13 {
		return 1
	}
	size := m.height - 11
	if len(m.modificationQueue) > 0 {
		size--
	}
	if m.safetyProgressView() != "" {
		size--
	}
	if m.compactRows() {
		size /= m.compactRowHeight()
	}
	if size < 1 {
		return 1
	}
	return size
}

func (m model) compactRowHeight() int {
	height := 2
	if m.width <= 0 {
		return height
	}
	for _, current := range m.visible {
		item := m.item(current)
		branch := item.Branch
		if branch == "" {
			branch = "detached"
		}
		lineWidth := ansi.StringWidth("      Branch: " + branch)
		wrapped := (lineWidth + m.width - 1) / m.width
		if 1+wrapped > height {
			height = 1 + wrapped
		}
	}
	return height
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
	if wasIdle {
		m.modificationTotal = 0
		m.modificationDone = 0
	}
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
		m.modificationTotal++
		queued[current] = true
	}
	if !wasIdle || len(m.modificationQueue) == 0 {
		return m, nil
	}
	current := m.modificationQueue[0]
	return m, scanModificationTime(m.generation, current, m.item(current).Path)
}

func (m model) deleteNext() tea.Cmd {
	current := m.deletionQueue[0]
	repo := m.repositories[current.repository]
	item := repo.Worktrees[current.worktree]
	return func() tea.Msg {
		return deletionProgressMsg{result: removeWorktree(context.Background(), repo, item, m.deleteBranches)}
	}
}

func (m model) returnToList() (tea.Model, tea.Cmd) {
	removed := make(map[string]bool)
	for _, result := range m.results {
		removed[result.Path] = result.Removed
	}
	var repositories []repository
	for _, repo := range m.repositories {
		var worktrees []worktree
		for _, item := range repo.Worktrees {
			if !removed[item.Path] {
				worktrees = append(worktrees, item)
			}
		}
		if len(worktrees) > 0 {
			repo.Worktrees = worktrees
			repositories = append(repositories, repo)
		}
	}
	refreshed := newModel(repositories)
	refreshed.width = m.width
	refreshed.height = m.height
	refreshed.query = m.query
	refreshed.safetyMode = m.safetyMode
	refreshed.generation = m.generation + 1
	refreshed.applyFilter()
	return refreshed, refreshed.Init()
}

func (m model) View() tea.View {
	var content string
	switch m.screen {
	case browseScreen:
		content = m.browseView()
	case reviewScreen:
		content = m.reviewView()
	case deletingScreen:
		content = m.deletingView()
	case resultsScreen:
		content = m.resultsView()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m model) browseView() string {
	var output strings.Builder
	fmt.Fprintf(&output, "\n\x1b[1mwt-man\x1b[0m  %d worktrees  %d selected  safety: %s\n",
		len(m.visible), len(m.selectedRows()), m.safetyMode.label())
	if progress := m.modificationProgressView(); progress != "" {
		output.WriteString(truncate(progress, m.width))
		output.WriteByte('\n')
	}
	if progress := m.safetyProgressView(); progress != "" {
		output.WriteString(truncate(progress, m.width))
		output.WriteByte('\n')
	}
	if m.filtering {
		fmt.Fprintf(&output, "Filter: %s█\n\n", m.query)
	} else if m.query != "" {
		fmt.Fprintf(&output, "Filter: %s  (/ to edit)\n\n", m.query)
	} else {
		output.WriteString("/ filter  s safety  r refresh  R refresh all  space select  a all  enter review  q quit\n\n")
	}

	end := m.offset + m.pageSize()
	if end > len(m.visible) {
		end = len(m.visible)
	}
	compact := m.compactRows()
	pathWidth := m.pathColumnWidth()
	var header string
	if compact {
		header = fmt.Sprintf("      %-*s %-10s %-10s %-8s %s",
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SAFE", "BRANCH")
	} else if pathWidth > 0 {
		header = fmt.Sprintf("      %-*s %-10s %-10s %-8s %-*s %s",
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SAFE", m.branchWidth, "BRANCH", "PATH")
	} else {
		header = fmt.Sprintf("      %-*s %-10s %-10s %-8s %s",
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SAFE", "BRANCH")
	}
	output.WriteString(truncate(header, m.width))
	output.WriteByte('\n')
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
		state, _ := m.safety(current)
		line := fmt.Sprintf("%s [%s] %-*s %-10s %-10s %-8s",
			pointer, checked, m.repositoryWidth, repoName, created, modified, safetyLabel(state))
		if compact {
			output.WriteString(truncate(line, m.width))
			output.WriteByte('\n')
			output.WriteString("      Branch: " + branch)
		} else {
			line += fmt.Sprintf(" %-*s", m.branchWidth, branch)
			if pathWidth > 0 {
				line += " " + truncate(item.Path, pathWidth)
			}
			output.WriteString(truncate(line, m.width))
		}
		output.WriteByte('\n')
	}
	if len(m.visible) == 0 {
		output.WriteString("No matching worktrees.\n")
	} else {
		current := m.visible[m.cursor]
		repo := m.repositories[current.repository]
		item := m.item(current)
		output.WriteByte('\n')
		output.WriteString(truncate("Path: "+item.Path, m.width))
		output.WriteByte('\n')
		state, reasons := m.safety(current)
		safetyDetails := "Safety: NOT SAFE"
		if state == safetySafe {
			safetyDetails = "Safety: SAFE"
		} else if state == safetyChecking {
			safetyDetails = "Safety: checking…"
		} else if len(reasons) > 0 {
			safetyDetails += " — " + strings.Join(reasons, "; ")
		}
		output.WriteString(truncate(safetyDetails, m.width))
		output.WriteByte('\n')
		branch := item.Branch
		if branch == "" {
			branch = "detached"
		}
		branchDetails := "Branch: " + branch
		if item.Missing {
			branchDetails += "  State: missing (Git record only)"
			if item.Prunable {
				branchDetails += "; prunable"
			}
		}
		if item.Locked {
			branchDetails += "  State: locked"
			if item.LockReason != "" {
				branchDetails += " (" + item.LockReason + ")"
			}
		}
		if item.MergeKnown {
			branchDetails += "  Merged into " + repo.MergeTarget + ": "
			if item.Merged {
				branchDetails += "yes"
			} else {
				branchDetails += "no"
			}
			if item.MergeSource != "" {
				branchDetails += " (" + item.MergeSource + ")"
			}
		}
		output.WriteString(truncate(branchDetails, m.width))
		output.WriteByte('\n')
	}
	return output.String()
}

func (m model) pathColumnWidth() int {
	if m.compactRows() {
		return 0
	}
	width := m.width - 38 - m.repositoryWidth - m.branchWidth
	if width < 12 {
		return 0
	}
	return width
}

func (m model) compactRows() bool {
	return 37+m.repositoryWidth+m.branchWidth > m.width
}

func scanModificationTime(generation int, current row, path string) tea.Cmd {
	return func() tea.Msg {
		modifiedAt := modificationTime(path)
		writeModificationCacheEntry(path, modifiedAt)
		return modificationTimeMsg{generation: generation, row: current, modifiedAt: modifiedAt}
	}
}

func scanChangesStatus(generation int, current row, path string) tea.Cmd {
	return func() tea.Msg {
		hasChanges, known := worktreeChanges(context.Background(), path)
		return changesStatusMsg{generation: generation, row: current, hasChanges: hasChanges, known: known}
	}
}

func scanMergeStatus(generation, repositoryIndex int, path string) tea.Cmd {
	return func() tea.Msg {
		merged, target := mergedBranches(context.Background(), path)
		return mergeStatusMsg{generation: generation, repository: repositoryIndex, merged: merged, target: target}
	}
}

func (m model) scanGitHubMergeStatus() tea.Cmd {
	repositories := make([]repository, len(m.repositories))
	for repositoryIndex, repo := range m.repositories {
		repositories[repositoryIndex] = repo
		repositories[repositoryIndex].Worktrees = append([]worktree(nil), repo.Worktrees...)
	}
	generation := m.generation
	return func() tea.Msg {
		authenticated, merged := githubMergedRows(context.Background(), repositories)
		return githubMergeStatusMsg{generation: generation, authenticated: authenticated, merged: merged}
	}
}

func (m model) modificationProgressView() string {
	if len(m.modificationQueue) == 0 {
		return ""
	}
	item := m.item(m.modificationQueue[0])
	return fmt.Sprintf("Date scan %s %d/%d  %s",
		progressBar(m.modificationDone, m.modificationTotal, 20), m.modificationDone, m.modificationTotal, item.Path)
}

func (m model) safetyProgressView() string {
	if len(m.changesQueue) > 0 {
		item := m.item(m.changesQueue[0])
		return fmt.Sprintf("Safety check %s %d/%d  %s",
			progressBar(m.changesDone, m.changesTotal, 20), m.changesDone, m.changesTotal, item.Path)
	}
	if len(m.mergeQueue) > 0 {
		repo := m.repositories[m.mergeQueue[0]]
		return fmt.Sprintf("Safety check %s %d/%d  %s",
			progressBar(m.mergeDone, m.mergeTotal, 20), m.mergeDone, m.mergeTotal, repo.Name)
	}
	if m.githubMergePending {
		return "Safety check: querying GitHub"
	}
	return ""
}

func (mode safetyMode) label() string {
	switch mode {
	case safeOnly:
		return "safe"
	case notSafeOnly:
		return "not safe"
	case checkingOnly:
		return "checking"
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
		state, reasons := m.safety(current)
		fmt.Fprintf(&output, "  [%s] %s  ", repo.Name, item.Path)
		if state == safetySafe {
			output.WriteString("\x1b[32mSAFE\x1b[0m")
		} else if state == safetyChecking {
			output.WriteString("\x1b[33mCHECKING\x1b[0m")
		} else {
			fmt.Fprintf(&output, "\x1b[31mNOT SAFE: %s\x1b[0m", strings.Join(reasons, "; "))
		}
		if item.Locked {
			output.WriteString("  \x1b[31mwill not delete")
			if item.LockReason != "" {
				output.WriteString(" (" + item.LockReason + ")")
			}
			output.WriteString("\x1b[0m")
		}
		if !item.Locked {
			if item.Missing {
				output.WriteString("  \x1b[33mmissing: delete Git record only\x1b[0m")
			} else {
				output.WriteString("  delete files and Git record")
			}
		}
		output.WriteByte('\n')
	}
	branches := "keep local branches"
	if m.deleteBranches {
		branches = "delete merged local branches (Git safe check)"
	}
	if m.selectedSafetyChecking() {
		fmt.Fprintf(&output, "\n[x] %s\n[b] back  Waiting for safety checks…  [q] quit\n", branches)
	} else {
		fmt.Fprintf(&output, "\n[x] %s\n[b] back  [D] delete permanently  [q] quit\n", branches)
	}
	return output.String()
}

func (m model) deletingView() string {
	completed := m.deletionTotal - len(m.deletionQueue)
	var output strings.Builder
	output.WriteString("\n\x1b[1mDeleting selected worktrees\x1b[0m\n\n")
	fmt.Fprintf(&output, "%s %d/%d\n", progressBar(completed, m.deletionTotal, 30), completed, m.deletionTotal)
	if m.deletionWaiting {
		item := m.item(m.modificationQueue[0])
		output.WriteString("\nFinishing the active date scan before deletion:\n")
		output.WriteString(truncate(item.Path, m.width))
	} else if len(m.deletionQueue) > 0 {
		item := m.item(m.deletionQueue[0])
		output.WriteString("\nDeleting:\n")
		output.WriteString(truncate(item.Path, m.width))
	}
	output.WriteByte('\n')
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
	output.WriteString("\nPress enter to return to the list, or q to quit.\n")
	return output.String()
}

func progressBar(completed, total, width int) string {
	filled := 0
	if total > 0 {
		filled = completed * width / total
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
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
