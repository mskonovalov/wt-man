package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	moveBrowserScreen
	moveConfirmScreen
	movingScreen
	moveResultScreen
)

const browseSessionDetailsHeight = 5

type sessionMode int

const (
	allSessions sessionMode = iota
	withUnarchivedSessions
	withoutUnarchivedSessions
)

type pullRequestMode int

const (
	allPullRequestStatuses pullRequestMode = iota
	closedOnly
	mergedOnly
	openOnly
	notApplicableOnly
)

type row struct {
	repository int
	worktree   int
}

type displayedSession struct {
	provider string
	detail   sessionDetail
}

type deletionResult struct {
	Path          string
	Branch        string
	Missing       bool
	Broken        bool
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

type githubPullRequestStatusMsg struct {
	generation    int
	authenticated bool
	pullRequests  map[row]pullRequestMatch
}

type gitRootsMsg struct {
	roots []string
	err   error
}

type repositoryDiscoveryMsg struct {
	generation      int
	commonDirectory string
	repository      repository
	found           bool
}

type sessionStatusMsg struct {
	claude      map[string]map[string]sessionDetail
	codex       map[string][]sessionDetail
	claudeKnown bool
	codexKnown  bool
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
	sessionMode         sessionMode
	pullRequestMode     pullRequestMode
	modificationQueue   []row
	modificationTotal   int
	modificationDone    int
	githubMergePending  bool
	githubAuthChecked   bool
	githubAuthAvailable bool
	deletionQueue       []row
	deletionTotal       int
	deletionWaiting     bool
	generation          int
	root                string
	discoveryPending    bool
	discoveryRoots      []string
	discoveryAllRoots   []string
	discoverySeen       map[string]bool
	discoveryTotal      int
	discoveryDone       int
	discoveryErr        error
	sessionsPending     bool
	claudeSessions      map[string]map[string]sessionDetail
	codexSessions       map[string][]sessionDetail
	claudeSessionsKnown bool
	codexSessionsKnown  bool
	moveRow             row
	moveBrowser         moveBrowser
	moveDestination     string
	moveResult          worktreeMoveResult
	moveWaiting         bool
	moveScansPaused     bool
}

func newDiscoveringModel(root string) model {
	m := newModel(nil)
	m.root = root
	m.discoveryPending = true
	m.discoverySeen = make(map[string]bool)
	m.sessionsPending = true
	return m
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
			if !repo.Worktrees[worktreeIndex].Missing && !repo.Worktrees[worktreeIndex].Broken {
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
	m.githubMergePending = true
	m.visible = append([]row(nil), m.rows...)
	return m
}

func (m model) Init() tea.Cmd {
	if m.discoveryPending && m.discoveryAllRoots == nil {
		return tea.Batch(findGitRootsCommand(m.root), scanSessionStatus())
	}
	var commands []tea.Cmd
	if len(m.modificationQueue) > 0 {
		current := m.modificationQueue[0]
		commands = append(commands, scanModificationTime(m.generation, current, m.item(current).Path))
	}
	if m.githubMergePending {
		commands = append(commands, m.scanGitHubMergeStatus())
	}
	return tea.Batch(commands...)
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case gitRootsMsg:
		if message.err != nil {
			m.discoveryPending = false
			m.discoveryErr = message.err
			return m, nil
		}
		m.discoveryRoots = append([]string(nil), message.roots...)
		m.discoveryAllRoots = append([]string(nil), message.roots...)
		m.discoveryTotal = len(message.roots)
		if len(m.discoveryRoots) == 0 {
			m.discoveryPending = false
			return m, nil
		}
		return m, scanRepository(m.generation, m.discoveryRoots[0], m.discoveryAllRoots)
	case repositoryDiscoveryMsg:
		if message.generation != m.generation || len(m.discoveryRoots) == 0 {
			return m, nil
		}
		m.discoveryRoots = m.discoveryRoots[1:]
		m.discoveryDone++
		if message.found && !m.discoverySeen[message.commonDirectory] {
			m.discoverySeen[message.commonDirectory] = true
			if !m.sessionsPending {
				repositories := []repository{message.repository}
				assignSessions(repositories, m.claudeSessions, m.codexSessions, m.claudeSessionsKnown, m.codexSessionsKnown)
				message.repository = repositories[0]
			}
			m.appendDiscoveredRepository(message.repository)
		}
		if len(m.discoveryRoots) > 0 {
			return m, scanRepository(m.generation, m.discoveryRoots[0], m.discoveryAllRoots)
		}
		return m.finishDiscovery()
	case sessionStatusMsg:
		m.sessionsPending = false
		m.claudeSessions = message.claude
		m.codexSessions = message.codex
		m.claudeSessionsKnown = message.claudeKnown
		m.codexSessionsKnown = message.codexKnown
		assignSessions(m.repositories, message.claude, message.codex, message.claudeKnown, message.codexKnown)
		if m.sessionMode != allSessions {
			m.applyFilter()
		}
		return m, nil
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
	case worktreeMoveMsg:
		return m.applyMoveResult(message), nil
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
		if m.screen == movingScreen && m.moveWaiting {
			m.moveWaiting = false
			return m, m.moveCommand()
		}
		if len(m.modificationQueue) == 0 {
			return m, nil
		}
		current := m.modificationQueue[0]
		return m, scanModificationTime(m.generation, current, m.item(current).Path)
	case githubPullRequestStatusMsg:
		if message.generation != m.generation {
			return m, nil
		}
		m.githubMergePending = false
		m.githubAuthChecked = true
		m.githubAuthAvailable = message.authenticated
		for _, current := range m.rows {
			item := &m.repositories[current.repository].Worktrees[current.worktree]
			item.PullRequestKnown = true
			item.PullRequestStatus = pullRequestUnmatched
			item.PullRequestTitle = ""
			item.PullRequestURL = ""
			item.PullRequestNumber = 0
		}
		for current, pullRequest := range message.pullRequests {
			item := &m.repositories[current.repository].Worktrees[current.worktree]
			item.PullRequestStatus = pullRequest.Status
			item.PullRequestTitle = pullRequest.Title
			item.PullRequestURL = pullRequest.URL
			item.PullRequestNumber = pullRequest.Number
		}
		if m.pullRequestMode != allPullRequestStatuses {
			m.applyFilter()
		}
		return m, nil
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" {
			if m.screen == deletingScreen || m.screen == movingScreen {
				return m, nil
			}
			return m, tea.Quit
		}
		return m.updateKey(message.String())
	}
	return m, nil
}

func (m model) updateKey(key string) (tea.Model, tea.Cmd) {
	if m.screen == moveBrowserScreen || m.screen == moveConfirmScreen || m.screen == movingScreen || m.screen == moveResultScreen {
		return m.updateMoveKey(key)
	}
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
	case "u":
		m.sessionMode = (m.sessionMode + 1) % 3
		m.applyFilter()
	case "p":
		m.pullRequestMode = (m.pullRequestMode + 1) % 5
		m.applyFilter()
	case "r":
		if len(m.visible) > 0 {
			current := m.visible[m.cursor]
			return m.queueModificationRefresh([]row{current})
		}
	case "R":
		return m.queueModificationRefresh(m.rows)
	case "M":
		if !m.discoveryPending {
			return m.beginMove()
		}
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
		if !m.discoveryPending && len(m.selectedRows()) > 0 {
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
		absenceKnown := item.Sessions.ClaudeKnown && item.Sessions.CodexKnown
		sessionMatches := m.sessionMode == allSessions ||
			(m.sessionMode == withUnarchivedSessions && hasUnarchived) ||
			(m.sessionMode == withoutUnarchivedSessions && absenceKnown && !hasUnarchived)
		pullRequestMatches := m.pullRequestMode == allPullRequestStatuses ||
			(m.pullRequestMode == closedOnly && item.PullRequestKnown && item.PullRequestStatus == pullRequestClosed) ||
			(m.pullRequestMode == mergedOnly && item.PullRequestKnown && item.PullRequestStatus == pullRequestMerged) ||
			(m.pullRequestMode == openOnly && item.PullRequestKnown && item.PullRequestStatus == pullRequestOpen) ||
			(m.pullRequestMode == notApplicableOnly && item.PullRequestKnown && item.PullRequestStatus == pullRequestUnmatched)
		if strings.Contains(haystack, query) && sessionMatches && pullRequestMatches {
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

func (m *model) appendDiscoveredRepository(repo repository) {
	repositoryIndex := len(m.repositories)
	m.repositories = append(m.repositories, repo)
	if width := utf8.RuneCountInString(repo.Name); width > m.repositoryWidth {
		m.repositoryWidth = width
	}
	for worktreeIndex, item := range repo.Worktrees {
		branch := item.Branch
		if branch == "" {
			branch = "detached"
		}
		if width := utf8.RuneCountInString(branch); width > m.branchWidth {
			m.branchWidth = width
		}
		m.rows = append(m.rows, row{repository: repositoryIndex, worktree: worktreeIndex})
	}
	m.applyFilter()
}

func (m model) finishDiscovery() (tea.Model, tea.Cmd) {
	sort.Slice(m.repositories, func(i, j int) bool {
		return m.repositories[i].Name < m.repositories[j].Name
	})
	loaded := newModel(m.repositories)
	loaded.selected = m.selected
	loaded.width = m.width
	loaded.height = m.height
	loaded.query = m.query
	loaded.filtering = m.filtering
	loaded.screen = m.screen
	loaded.sessionMode = m.sessionMode
	loaded.pullRequestMode = m.pullRequestMode
	loaded.generation = m.generation
	loaded.root = m.root
	loaded.sessionsPending = m.sessionsPending
	loaded.claudeSessions = m.claudeSessions
	loaded.codexSessions = m.codexSessions
	loaded.claudeSessionsKnown = m.claudeSessionsKnown
	loaded.codexSessionsKnown = m.codexSessionsKnown
	loaded.applyFilter()
	return loaded, loaded.Init()
}

func (m model) pageSize() int {
	if m.height < 13 {
		return 1
	}
	size := m.height - 13
	if len(m.modificationQueue) > 0 {
		size--
	}
	size -= m.sessionDetailsHeight()
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
		lineWidth := ansi.StringWidth("      Branch: " + branch + "  PR: " + pullRequestLabel(item))
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
		if item.Missing || item.Broken || queued[current] {
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
	refreshed.sessionMode = m.sessionMode
	refreshed.pullRequestMode = m.pullRequestMode
	refreshed.generation = m.generation + 1
	refreshed.root = m.root
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
	case moveBrowserScreen:
		content = m.moveBrowserView()
	case moveConfirmScreen:
		content = m.moveConfirmView()
	case movingScreen:
		content = m.movingView()
	case moveResultScreen:
		content = m.moveResultView()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m model) browseView() string {
	var output strings.Builder
	fmt.Fprintf(&output, "\n\x1b[1mwt-man\x1b[0m  %d worktrees  %d selected  sessions: %s  PR: %s\n",
		len(m.visible), len(m.selectedRows()), m.sessionMode.label(), m.pullRequestMode.label())
	if progress := m.discoveryProgressView(); progress != "" {
		output.WriteString(truncate(progress, m.width))
		output.WriteByte('\n')
	}
	if progress := m.modificationProgressView(); progress != "" {
		output.WriteString(truncate(progress, m.width))
		output.WriteByte('\n')
	}
	output.WriteString(truncate(m.statusProgressView(), m.width))
	output.WriteByte('\n')
	if m.filtering {
		fmt.Fprintf(&output, "Filter: %s█\n\n", m.query)
	} else if m.query != "" {
		fmt.Fprintf(&output, "Filter: %s  (/ to edit)\n\n", m.query)
	} else {
		output.WriteString("/ filter  u sessions  p PR  M move  r refresh  R refresh all  space select  a all  enter review  q quit\n\n")
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
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SESSIONS", "PR")
	} else if pathWidth > 0 {
		header = fmt.Sprintf("      %-*s %-10s %-10s %-8s %-6s %-*s %s",
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SESSIONS", "PR", m.branchWidth, "BRANCH", "PATH")
	} else {
		header = fmt.Sprintf("      %-*s %-10s %-10s %-8s %-6s %s",
			m.repositoryWidth, "REPOSITORY", "CREATED", "MODIFIED", "SESSIONS", "PR", "BRANCH")
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
		} else if item.Broken {
			created = "broken"
		} else if !item.CreatedAt.IsZero() {
			created = item.CreatedAt.Format("2006-01-02")
		}
		modified := "scanning"
		if item.Missing {
			modified = "missing"
		} else if item.Broken {
			modified = "broken"
		} else if !item.ModifiedAt.IsZero() {
			modified = item.ModifiedAt.Format("2006-01-02")
		} else if len(m.modificationQueue) == 0 {
			modified = "unknown"
		}
		branch := item.Branch
		if branch == "" {
			branch = "detached"
		}
		sessions := sessionLabel(item.Sessions)
		pullRequest := pullRequestLabel(item)
		line := fmt.Sprintf("%s [%s] %-*s %-10s %-10s %-8s %-6s",
			pointer, checked, m.repositoryWidth, repoName, created, modified, sessions, pullRequest)
		highlighted := index == m.cursor
		if compact {
			output.WriteString(tableRowLine(line, m.width, highlighted))
			output.WriteByte('\n')
			output.WriteString(tableRowLine("      Branch: "+branch+"  PR: "+pullRequest, m.width, highlighted))
		} else {
			line += fmt.Sprintf(" %-*s", m.branchWidth, branch)
			if pathWidth > 0 {
				line += " " + truncate(item.Path, pathWidth)
			}
			output.WriteString(tableRowLine(line, m.width, highlighted))
		}
		output.WriteByte('\n')
	}
	if len(m.visible) == 0 {
		if m.discoveryPending {
			output.WriteString("Waiting for linked worktrees...\n")
		} else if m.discoveryErr != nil {
			fmt.Fprintf(&output, "Repository scan failed: %v\n", m.discoveryErr)
		} else if len(m.rows) == 0 {
			output.WriteString("No linked Git worktrees found.\n")
		} else {
			output.WriteString("No matching worktrees.\n")
		}
	} else {
		current := m.visible[m.cursor]
		item := m.item(current)
		output.WriteByte('\n')
		output.WriteString(truncate("Path: "+item.Path, m.width))
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
		} else if item.Broken {
			branchDetails += "  State: broken (Git metadata missing; leftover files remain)"
		}
		if item.Locked {
			branchDetails += "  State: locked"
			if item.LockReason != "" {
				branchDetails += " (" + item.LockReason + ")"
			}
		}
		output.WriteString(truncate(branchDetails, m.width))
		output.WriteByte('\n')
		pullRequestDetails := "PR: [" + pullRequestLabel(item) + "]"
		if item.PullRequestTitle != "" {
			pullRequestTitle := item.PullRequestTitle
			if item.PullRequestNumber > 0 {
				pullRequestTitle = fmt.Sprintf("#%d %s", item.PullRequestNumber, pullRequestTitle)
			}
			if item.PullRequestURL != "" {
				pullRequestTitle = ansi.SetHyperlink(item.PullRequestURL) + ansi.Style{}.Underline(true).Styled(pullRequestTitle) + ansi.ResetHyperlink()
			}
			pullRequestDetails += " - " + pullRequestTitle
		}
		output.WriteString(ansi.Truncate(pullRequestDetails, m.width, "…"))
		output.WriteByte('\n')
		sessionDetails := sessionDetailsView(item.Sessions, m.width)
		output.WriteString(sessionDetails)
		output.WriteString(strings.Repeat("\n", browseSessionDetailsHeight-strings.Count(sessionDetails, "\n")))
	}
	return output.String()
}

func sessionLabel(sessions sessionCounts) string {
	claude := "?"
	if sessions.ClaudeKnown {
		claude = fmt.Sprint(sessions.Claude)
	}
	codex := "?"
	if sessions.CodexKnown {
		codex = fmt.Sprint(sessions.Codex)
	}
	warning := ""
	if sessions.Claude+sessions.Codex > 0 || !sessions.ClaudeKnown || !sessions.CodexKnown {
		warning = " !"
	}
	return fmt.Sprintf("C%s X%s%s", claude, codex, warning)
}

func (m model) sessionDetailsHeight() int {
	if len(m.visible) == 0 {
		return 0
	}
	return browseSessionDetailsHeight
}

func sessionDetailsView(sessions sessionCounts, width int) string {
	var details []displayedSession
	for _, detail := range sessions.ClaudeSessions {
		details = append(details, displayedSession{provider: "Claude", detail: detail})
	}
	for _, detail := range sessions.CodexSessions {
		details = append(details, displayedSession{provider: "Codex", detail: detail})
	}
	if len(details) == 0 {
		return ""
	}
	sort.SliceStable(details, func(i, j int) bool {
		return details[i].detail.UpdatedAt.After(details[j].detail.UpdatedAt)
	})
	var output strings.Builder
	output.WriteString("Sessions:\n")
	for _, session := range details[:min(len(details), 3)] {
		title := cleanSessionText(session.detail.Title)
		if title == "" {
			title = "Untitled session"
		}
		prefix := fmt.Sprintf("  %s: ", session.provider)
		metadata := ""
		if model := cleanSessionText(session.detail.Model); model != "" {
			metadata += " · " + model
		}
		if !session.detail.UpdatedAt.IsZero() {
			metadata += " · active " + session.detail.UpdatedAt.Format("2006-01-02 15:04")
		}
		title = ansi.Truncate(title, max(width-ansi.StringWidth(prefix)-ansi.StringWidth(metadata)-2, 1), "…")
		sessionTitle := fmt.Sprintf("%q", title)
		if session.detail.URL != "" {
			sessionTitle = ansi.SetHyperlink(session.detail.URL) + ansi.Style{}.Underline(true).Styled(sessionTitle) + ansi.ResetHyperlink()
		}
		line := prefix + sessionTitle + metadata
		output.WriteString(ansi.Truncate(line, width, "…"))
		output.WriteByte('\n')
	}
	if len(details) > 3 {
		output.WriteString(fmt.Sprintf("  +%d more\n", len(details)-3))
	}
	return output.String()
}

func cleanSessionText(value string) string {
	return strings.Join(strings.Fields(ansi.Strip(value)), " ")
}

func pullRequestLabel(item worktree) string {
	if item.Branch != "" && !item.PullRequestKnown {
		return "?"
	}
	switch item.PullRequestStatus {
	case pullRequestClosed:
		return "closed"
	case pullRequestMerged:
		return "merged"
	case pullRequestOpen:
		return "open"
	default:
		return "n/a"
	}
}

func (m model) pathColumnWidth() int {
	if m.compactRows() {
		return 0
	}
	width := m.width - 46 - m.repositoryWidth - m.branchWidth
	if width < 12 {
		return 0
	}
	return width
}

func (m model) compactRows() bool {
	return 45+m.repositoryWidth+m.branchWidth > m.width
}

func scanModificationTime(generation int, current row, path string) tea.Cmd {
	return func() tea.Msg {
		modifiedAt := modificationTime(path)
		writeModificationCacheEntry(path, modifiedAt)
		return modificationTimeMsg{generation: generation, row: current, modifiedAt: modifiedAt}
	}
}

func findGitRootsCommand(root string) tea.Cmd {
	return func() tea.Msg {
		root, err := canonicalPath(root)
		if err != nil {
			return gitRootsMsg{err: fmt.Errorf("resolve root: %w", err)}
		}
		roots, err := findGitRoots(root)
		return gitRootsMsg{roots: roots, err: err}
	}
}

func scanRepository(generation int, gitRoot string, gitRoots []string) tea.Cmd {
	return func() tea.Msg {
		commonDirectory, repo, found := discoverRepository(context.Background(), gitRoot, gitRoots)
		return repositoryDiscoveryMsg{
			generation: generation, commonDirectory: commonDirectory, repository: repo, found: found,
		}
	}
}

func scanSessionStatus() tea.Cmd {
	return func() tea.Msg {
		var claude map[string]map[string]sessionDetail
		var codex map[string][]sessionDetail
		var claudeKnown bool
		var codexKnown bool
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			claude, claudeKnown = readClaudeSessions()
		}()
		go func() {
			defer wait.Done()
			codex, codexKnown = readCodexSessions(context.Background())
		}()
		wait.Wait()
		return sessionStatusMsg{claude: claude, codex: codex, claudeKnown: claudeKnown, codexKnown: codexKnown}
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
		authenticated, pullRequests := githubPullRequestRows(context.Background(), repositories)
		return githubPullRequestStatusMsg{generation: generation, authenticated: authenticated, pullRequests: pullRequests}
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

func (m model) discoveryProgressView() string {
	if !m.discoveryPending {
		return ""
	}
	if m.discoveryTotal == 0 {
		return "Repository scan: finding Git repositories under " + m.root
	}
	return fmt.Sprintf("Repository scan %s %d/%d", progressBar(m.discoveryDone, m.discoveryTotal, 20), m.discoveryDone, m.discoveryTotal)
}

func (m model) statusProgressView() string {
	if m.discoveryPending {
		return "PR check: waiting for repository scan"
	}
	if m.githubMergePending {
		return "PR check: querying GitHub"
	}
	if m.githubAuthChecked && !m.githubAuthAvailable {
		return "Warning: GitHub authentication unavailable; PR status is n/a. Set GH_TOKEN or run gh auth login."
	}
	return "PR check: complete"
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

func (mode pullRequestMode) label() string {
	switch mode {
	case closedOnly:
		return "closed"
	case mergedOnly:
		return "merged"
	case openOnly:
		return "open"
	case notApplicableOnly:
		return "n/a"
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
		if item.Locked {
			output.WriteString("  \x1b[31mLOCKED: will not delete")
			if item.LockReason != "" {
				output.WriteString(" (" + item.LockReason + ")")
			}
			output.WriteString("\x1b[0m")
		}
		if !item.Locked {
			if item.Missing {
				output.WriteString("  \x1b[33mmissing: delete Git record only\x1b[0m")
			} else if item.Broken {
				output.WriteString("  \x1b[33mbroken: delete leftover files and Git record\x1b[0m")
			} else {
				output.WriteString("  delete files and Git record")
			}
		}
		if item.Sessions.Claude+item.Sessions.Codex > 0 {
			var active []string
			if item.Sessions.Claude > 0 {
				active = append(active, fmt.Sprintf("Claude %d", item.Sessions.Claude))
			}
			if item.Sessions.Codex > 0 {
				active = append(active, fmt.Sprintf("Codex %d", item.Sessions.Codex))
			}
			fmt.Fprintf(&output, "  \x1b[33m%s unarchived\x1b[0m", strings.Join(active, ", "))
		}
		if !item.Sessions.ClaudeKnown || !item.Sessions.CodexKnown {
			var unknown []string
			if !item.Sessions.ClaudeKnown {
				unknown = append(unknown, "Claude")
			}
			if !item.Sessions.CodexKnown {
				unknown = append(unknown, "Codex")
			}
			fmt.Fprintf(&output, "  \x1b[33m%s session status unknown\x1b[0m", strings.Join(unknown, "/"))
		}
		output.WriteByte('\n')
	}
	branches := "keep local branches"
	if m.deleteBranches {
		branches = "delete merged local branches (Git safe check)"
	}
	fmt.Fprintf(&output, "\n[x] %s\n[b] back  [D] delete permanently  [q] quit\n", branches)
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
		} else if result.Broken {
			output.WriteString(" (deleted broken worktree files and Git record)")
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

func tableRowLine(value string, width int, highlighted bool) string {
	value = truncate(value, width)
	if highlighted {
		return "\x1b[1;7m" + value + "\x1b[0m"
	}
	return value
}
