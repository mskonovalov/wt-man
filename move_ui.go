package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type moveDirectoryChoice struct {
	label      string
	path       string
	chooseHere bool
}

type moveBrowser struct {
	directory  string
	choices    []moveDirectoryChoice
	cursor     int
	offset     int
	showHidden bool
	err        error
}

type worktreeMoveMsg struct {
	row    row
	result worktreeMoveResult
}

func loadMoveBrowser(directory, worktreeName string, showHidden bool) (moveBrowser, error) {
	directory, err := canonicalPath(directory)
	if err != nil {
		return moveBrowser{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return moveBrowser{}, err
	}
	browser := moveBrowser{directory: directory, showHidden: showHidden}
	browser.choices = append(browser.choices, moveDirectoryChoice{
		label:      "Move here as " + worktreeName,
		path:       directory,
		chooseHere: true,
	})
	parent := filepath.Dir(directory)
	if parent != directory {
		browser.choices = append(browser.choices, moveDirectoryChoice{label: "../", path: parent})
	}
	var directories []moveDirectoryChoice
	for _, entry := range entries {
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		isDirectory := entry.IsDir()
		if !isDirectory && entry.Type()&os.ModeSymlink != 0 {
			if info, statErr := os.Stat(path); statErr == nil {
				isDirectory = info.IsDir()
			}
		}
		if isDirectory {
			directories = append(directories, moveDirectoryChoice{label: entry.Name() + "/", path: path})
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].label) < strings.ToLower(directories[j].label)
	})
	browser.choices = append(browser.choices, directories...)
	return browser, nil
}

func (m model) beginMove() (tea.Model, tea.Cmd) {
	if len(m.visible) == 0 {
		return m, nil
	}
	m.moveBrowser = moveBrowser{}
	m.moveDestination = ""
	m.moveResult = worktreeMoveResult{}
	m.moveRow = m.visible[m.cursor]
	item := m.item(m.moveRow)
	repo := m.repositories[m.moveRow.repository]
	if m.sessionsPending {
		m.moveResult = worktreeMoveResult{Source: item.Path, Err: fmt.Errorf("cannot move until the session scan finishes")}
		m.screen = moveResultScreen
		return m, nil
	}
	if err := worktreeMoveUnavailable(repo, item); err != nil {
		m.moveResult = worktreeMoveResult{Source: item.Path, Err: err}
		m.screen = moveResultScreen
		return m, nil
	}
	start := m.root
	if start == "" {
		start = filepath.Dir(item.Path)
	}
	browser, err := loadMoveBrowser(start, filepath.Base(filepath.Clean(item.Path)), false)
	if err != nil {
		m.moveResult = worktreeMoveResult{Source: item.Path, Err: fmt.Errorf("open destination browser: %w", err)}
		m.screen = moveResultScreen
		return m, nil
	}
	m.moveBrowser = browser
	m.moveDestination = ""
	m.moveResult = worktreeMoveResult{}
	m.screen = moveBrowserScreen
	return m, nil
}

func (m model) updateMoveKey(key string) (tea.Model, tea.Cmd) {
	switch m.screen {
	case moveBrowserScreen:
		switch key {
		case "q":
			return m, tea.Quit
		case "esc", "b":
			return m.returnFromMove()
		case "up", "k":
			if m.moveBrowser.cursor > 0 {
				m.moveBrowser.cursor--
			}
		case "down", "j":
			if m.moveBrowser.cursor < len(m.moveBrowser.choices)-1 {
				m.moveBrowser.cursor++
			}
		case "pgup":
			m.moveBrowser.cursor -= m.moveBrowserPageSize()
			if m.moveBrowser.cursor < 0 {
				m.moveBrowser.cursor = 0
			}
		case "pgdown":
			m.moveBrowser.cursor += m.moveBrowserPageSize()
			if m.moveBrowser.cursor >= len(m.moveBrowser.choices) {
				m.moveBrowser.cursor = len(m.moveBrowser.choices) - 1
			}
		case "left", "backspace", "ctrl+h":
			return m.openMoveDirectory(filepath.Dir(m.moveBrowser.directory))
		case "~":
			home, err := os.UserHomeDir()
			if err != nil {
				m.moveBrowser.err = err
				return m, nil
			}
			return m.openMoveDirectory(home)
		case ".":
			m.moveBrowser.showHidden = !m.moveBrowser.showHidden
			return m.openMoveDirectory(m.moveBrowser.directory)
		case "enter", "right", "l":
			if len(m.moveBrowser.choices) == 0 {
				return m, nil
			}
			choice := m.moveBrowser.choices[m.moveBrowser.cursor]
			if !choice.chooseHere {
				return m.openMoveDirectory(choice.path)
			}
			repo := m.repositories[m.moveRow.repository]
			item := m.item(m.moveRow)
			destination, err := worktreeMoveDestination(repo, item, m.moveBrowser.directory)
			if err != nil {
				m.moveBrowser.err = err
				return m, nil
			}
			m.moveDestination = destination
			m.moveBrowser.err = nil
			m.screen = moveConfirmScreen
			return m, nil
		}
		m.ensureMoveBrowserCursorVisible()
		return m, nil
	case moveConfirmScreen:
		switch key {
		case "q":
			return m, tea.Quit
		case "esc":
			return m.returnFromMove()
		case "b":
			m.screen = moveBrowserScreen
		case "enter", "M":
			m.screen = movingScreen
			if len(m.modificationQueue) > 0 {
				m.moveWaiting = true
				m.moveScansPaused = true
				return m, nil
			}
			return m, m.moveCommand()
		}
		return m, nil
	case movingScreen:
		return m, nil
	case moveResultScreen:
		switch key {
		case "q":
			return m, tea.Quit
		case "b":
			if m.moveResult.Err != nil && m.moveBrowser.directory != "" {
				m.screen = moveBrowserScreen
				command := m.resumeMovePausedScans()
				return m, command
			}
			return m.returnFromMove()
		case "enter", "esc":
			return m.returnFromMove()
		}
		return m, nil
	}
	return m, nil
}

func (m model) openMoveDirectory(directory string) (tea.Model, tea.Cmd) {
	item := m.item(m.moveRow)
	browser, err := loadMoveBrowser(directory, filepath.Base(filepath.Clean(item.Path)), m.moveBrowser.showHidden)
	if err != nil {
		m.moveBrowser.err = err
		return m, nil
	}
	m.moveBrowser = browser
	return m, nil
}

func (m model) moveCommand() tea.Cmd {
	current := m.moveRow
	repo := m.repositories[current.repository]
	item := m.item(current)
	parent := m.moveBrowser.directory
	return func() tea.Msg {
		return worktreeMoveMsg{row: current, result: moveWorktree(context.Background(), repo, item, parent)}
	}
}

func (m model) applyMoveResult(message worktreeMoveMsg) model {
	m.moveWaiting = false
	m.moveResult = message.result
	if message.result.Err == nil {
		item := &m.repositories[message.row.repository].Worktrees[message.row.worktree]
		oldPath := item.Path
		item.Path = message.result.Destination
		item.CreatedAt = creationTime(item.Path)
		if m.selected[oldPath] {
			delete(m.selected, oldPath)
			m.selected[item.Path] = true
		}
		m.applyFilter()
	}
	m.screen = moveResultScreen
	return m
}

func (m model) returnFromMove() (tea.Model, tea.Cmd) {
	m.screen = browseScreen
	m.moveDestination = ""
	m.moveResult = worktreeMoveResult{}
	m.moveBrowser = moveBrowser{}
	command := m.resumeMovePausedScans()
	return m, command
}

func (m *model) resumeMovePausedScans() tea.Cmd {
	if !m.moveScansPaused {
		return nil
	}
	m.moveScansPaused = false
	if len(m.modificationQueue) == 0 {
		return nil
	}
	current := m.modificationQueue[0]
	return scanModificationTime(m.generation, current, m.item(current).Path)
}

func (m model) moveBrowserPageSize() int {
	size := m.height - 10
	if size < 1 {
		return 1
	}
	return size
}

func (m *model) ensureMoveBrowserCursorVisible() {
	if m.moveBrowser.cursor < m.moveBrowser.offset {
		m.moveBrowser.offset = m.moveBrowser.cursor
	}
	pageSize := m.moveBrowserPageSize()
	if m.moveBrowser.cursor >= m.moveBrowser.offset+pageSize {
		m.moveBrowser.offset = m.moveBrowser.cursor - pageSize + 1
	}
}

func (m model) moveBrowserView() string {
	item := m.item(m.moveRow)
	var output strings.Builder
	output.WriteString("\n\x1b[1mMove worktree\x1b[0m\n\n")
	output.WriteString(truncate("From: "+item.Path, m.width))
	output.WriteByte('\n')
	destination := filepath.Join(m.moveBrowser.directory, filepath.Base(filepath.Clean(item.Path)))
	output.WriteString(truncate("To:   "+destination, m.width))
	output.WriteString("\n\n")
	if m.moveBrowser.err != nil {
		output.WriteString(truncate("\x1b[31m"+m.moveBrowser.err.Error()+"\x1b[0m", m.width))
		output.WriteByte('\n')
	}
	end := min(len(m.moveBrowser.choices), m.moveBrowser.offset+m.moveBrowserPageSize())
	for index := m.moveBrowser.offset; index < end; index++ {
		pointer := "  "
		if index == m.moveBrowser.cursor {
			pointer = "› "
		}
		output.WriteString(truncate(pointer+m.moveBrowser.choices[index].label, m.width))
		output.WriteByte('\n')
	}
	visibility := "show hidden"
	if m.moveBrowser.showHidden {
		visibility = "hide hidden"
	}
	fmt.Fprintf(&output, "\n↑/↓ navigate  enter open/select  ← parent  . %s  ~ home  esc cancel\n", visibility)
	return output.String()
}

func (m model) moveConfirmView() string {
	item := m.item(m.moveRow)
	var output strings.Builder
	output.WriteString("\n\x1b[1mConfirm move\x1b[0m\n\n")
	output.WriteString(truncate("From: "+item.Path, m.width))
	output.WriteByte('\n')
	output.WriteString(truncate("To:   "+m.moveDestination, m.width))
	output.WriteByte('\n')
	if hints := moveSessionHints(item.Sessions, m.moveDestination); len(hints) > 0 {
		output.WriteString("\n\x1b[33mResume sessions after moving:\x1b[0m\n")
		for _, hint := range hints {
			output.WriteString("  " + hint)
			output.WriteByte('\n')
		}
	}
	output.WriteString("\n[enter] move  [b] back  [esc] cancel\n")
	return output.String()
}

func (m model) movingView() string {
	var output strings.Builder
	output.WriteString("\n\x1b[1mMoving worktree\x1b[0m\n\n")
	if m.moveWaiting {
		output.WriteString("Finishing the active date scan before moving…\n")
	} else {
		output.WriteString(truncate("Moving to: "+m.moveDestination, m.width))
		output.WriteByte('\n')
	}
	return output.String()
}

func (m model) moveResultView() string {
	var output strings.Builder
	if m.moveResult.Err != nil {
		output.WriteString("\n\x1b[1mMove failed\x1b[0m\n\n")
		output.WriteString(truncate(m.moveResult.Err.Error(), m.width))
		if m.moveBrowser.directory != "" {
			output.WriteString("\n\n[b] choose another folder  [enter] return to list  [q] quit\n")
		} else {
			output.WriteString("\n\n[enter] return to list  [q] quit\n")
		}
		return output.String()
	}
	output.WriteString("\n\x1b[1mWorktree moved\x1b[0m\n\n")
	output.WriteString(truncate(m.moveResult.Source+" → "+m.moveResult.Destination, m.width))
	if hints := moveSessionHints(m.item(m.moveRow).Sessions, m.moveResult.Destination); len(hints) > 0 {
		output.WriteString("\n\nResume sessions:\n")
		for _, hint := range hints {
			output.WriteString("  " + hint)
			output.WriteByte('\n')
		}
	}
	output.WriteString("\n\n[enter] return to list  [q] quit\n")
	return output.String()
}

func moveSessionHints(sessions worktreeSessions, destination string) []string {
	var hints []string
	for _, provider := range sessions.Providers {
		if provider.Provider == nil {
			continue
		}
		for _, session := range provider.visibleSessions() {
			hints = append(hints, provider.Provider.MoveHint(session, destination))
		}
	}
	return hints
}
