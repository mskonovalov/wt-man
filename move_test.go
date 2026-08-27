package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createMoveTestWorktree(t *testing.T) (repository, worktree, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	sourcePath := filepath.Join(root, "feature-worktree")
	if err := os.Mkdir(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", "feature/move", sourcePath); err != nil {
		t.Fatal(err)
	}
	return repository{Name: "repo", MainPath: repoPath}, worktree{Path: sourcePath, Branch: "feature/move"}, root
}

func TestMoveWorktreePreservesLocalFiles(t *testing.T) {
	repo, item, root := createMoveTestWorktree(t)
	destinationParent := filepath.Join(root, "destination")
	if err := os.Mkdir(destinationParent, 0o755); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(item.Path, "untracked.txt")
	if err := os.WriteFile(localFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := moveWorktree(context.Background(), repo, item, destinationParent)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	wantDestination := filepath.Join(destinationParent, filepath.Base(item.Path))
	wantDestination, _ = canonicalPath(wantDestination)
	if result.Destination != wantDestination {
		t.Fatalf("destination=%q, want %q", result.Destination, wantDestination)
	}
	if _, err := os.Stat(item.Path); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(wantDestination, "untracked.txt"))
	if err != nil || string(data) != "keep me" {
		t.Fatalf("untracked file was not preserved: %q, %v", data, err)
	}
	output, err := git(context.Background(), repo.MainPath, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, item.Path) || !strings.Contains(output, wantDestination) {
		t.Fatalf("Git worktree record was not updated: %s", output)
	}
}

func TestWorktreeMoveDestinationRejectsUnsafeTargets(t *testing.T) {
	repo, item, root := createMoveTestWorktree(t)
	parent := filepath.Join(root, "destination")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, changed := range map[string]worktree{
		"missing": {Path: item.Path, Missing: true},
		"broken":  {Path: item.Path, Broken: true},
		"locked":  {Path: item.Path, Locked: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := worktreeMoveDestination(repo, changed, parent); err == nil {
				t.Fatal("unsafe worktree was accepted")
			}
		})
	}
	if _, err := worktreeMoveDestination(repo, item, filepath.Dir(item.Path)); err == nil || !strings.Contains(err.Error(), "already in this folder") {
		t.Fatalf("same destination returned %v", err)
	}
	inside := filepath.Join(item.Path, "nested")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := worktreeMoveDestination(repo, item, inside); err == nil || !strings.Contains(err.Error(), "inside itself") {
		t.Fatalf("nested destination returned %v", err)
	}
	existing := filepath.Join(parent, filepath.Base(item.Path))
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := worktreeMoveDestination(repo, item, parent); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing destination returned %v", err)
	}
}

func TestLoadMoveBrowserShowsOnlyDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"visible", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	browser, err := loadMoveBrowser(root, "worktree", false)
	if err != nil {
		t.Fatal(err)
	}
	labels := moveChoiceLabels(browser.choices)
	if !strings.Contains(labels, "Move here as worktree") || !strings.Contains(labels, "visible/") || strings.Contains(labels, ".hidden/") || strings.Contains(labels, "file.txt") {
		t.Fatalf("unexpected browser choices: %s", labels)
	}
	browser, err = loadMoveBrowser(root, "worktree", true)
	if err != nil {
		t.Fatal(err)
	}
	if labels := moveChoiceLabels(browser.choices); !strings.Contains(labels, ".hidden/") {
		t.Fatalf("hidden directory was not shown: %s", labels)
	}
}

func TestMoveBrowserFlow(t *testing.T) {
	repo, item, root := createMoveTestWorktree(t)
	destinationParent := filepath.Join(root, "destination")
	if err := os.Mkdir(destinationParent, 0o755); err != nil {
		t.Fatal(err)
	}
	m := newModel([]repository{{Name: repo.Name, MainPath: repo.MainPath, Worktrees: []worktree{item}}})
	m.root = destinationParent
	m.modificationQueue = nil

	updated, command := m.updateKey("M")
	m = updated.(model)
	if command != nil || m.screen != moveBrowserScreen || !strings.Contains(m.moveBrowserView(), "Move here as feature-worktree") {
		t.Fatalf("move browser did not open: screen=%v view=%q", m.screen, m.moveBrowserView())
	}
	updated, command = m.updateKey("enter")
	m = updated.(model)
	if command != nil || m.screen != moveConfirmScreen {
		t.Fatalf("move confirmation did not open: screen=%v", m.screen)
	}
	updated, command = m.updateKey("enter")
	m = updated.(model)
	if command == nil || m.screen != movingScreen {
		t.Fatalf("move did not start: screen=%v", m.screen)
	}
	message := command()
	updated, command = m.Update(message)
	m = updated.(model)
	if command != nil || m.screen != moveResultScreen || m.moveResult.Err != nil {
		t.Fatalf("move did not finish: screen=%v result=%#v", m.screen, m.moveResult)
	}
	wantDestination := filepath.Join(destinationParent, filepath.Base(item.Path))
	wantDestination, _ = canonicalPath(wantDestination)
	if m.item(m.rows[0]).Path != wantDestination || !strings.Contains(m.moveResultView(), "Worktree moved") {
		t.Fatalf("model path was not updated: %#v", m.item(m.rows[0]))
	}
	updated, _ = m.updateKey("enter")
	if updated.(model).screen != browseScreen {
		t.Fatal("move result did not return to the list")
	}
}

func TestMoveWaitsForActiveModificationScan(t *testing.T) {
	repo, item, root := createMoveTestWorktree(t)
	destinationParent := filepath.Join(root, "destination")
	if err := os.Mkdir(destinationParent, 0o755); err != nil {
		t.Fatal(err)
	}
	m := newModel([]repository{{Name: repo.Name, MainPath: repo.MainPath, Worktrees: []worktree{item}}})
	m.root = destinationParent
	updated, _ := m.updateKey("M")
	m = updated.(model)
	updated, _ = m.updateKey("enter")
	m = updated.(model)
	updated, command := m.updateKey("enter")
	m = updated.(model)
	if command != nil || !m.moveWaiting || !m.moveScansPaused || m.screen != movingScreen {
		t.Fatalf("move did not wait for date scan: %#v", m)
	}

	current := m.modificationQueue[0]
	updated, command = m.Update(modificationTimeMsg{generation: m.generation, row: current})
	m = updated.(model)
	if command == nil || m.moveWaiting {
		t.Fatal("move did not start after the active scan completed")
	}
}

func TestMoveConfirmationWarnsAboutSessions(t *testing.T) {
	repo, item, root := createMoveTestWorktree(t)
	item.Sessions = sessionCounts{Claude: 1, Codex: 1, ClaudeKnown: true, CodexKnown: true}
	m := newModel([]repository{{Name: repo.Name, MainPath: repo.MainPath, Worktrees: []worktree{item}}})
	m.root = root
	m.moveRow = m.rows[0]
	m.moveDestination = filepath.Join(root, "elsewhere", filepath.Base(item.Path))
	m.screen = moveConfirmScreen
	if view := m.moveConfirmView(); !strings.Contains(view, "2 unarchived session(s)") {
		t.Fatalf("session warning was not rendered: %q", view)
	}
}

func moveChoiceLabels(choices []moveDirectoryChoice) string {
	var labels []string
	for _, choice := range choices {
		labels = append(labels, choice.label)
	}
	return strings.Join(labels, "\n")
}
