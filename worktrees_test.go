package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWorktrees(t *testing.T) {
	output := "worktree /tmp/repo\n" +
		"HEAD abc123\n" +
		"branch refs/heads/main\n\n" +
		"worktree /tmp/repo-feature\n" +
		"HEAD def456\n" +
		"branch refs/heads/feature/test\n" +
		"locked reason\n\n" +
		"worktree /tmp/repo-detached\n" +
		"HEAD fed321\n" +
		"detached\n" +
		"prunable gitdir file points to non-existent location\n"

	items, err := parseWorktrees(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d worktrees, want 3", len(items))
	}
	if items[1].Head != "def456" || items[1].Branch != "feature/test" || !items[1].Locked {
		t.Fatalf("unexpected linked worktree: %#v", items[1])
	}
	if !items[2].Detached || !items[2].Prunable {
		t.Fatalf("unexpected detached worktree: %#v", items[2])
	}
}

func TestGitHubRepositoryFromOrigin(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	if _, err := git(ctx, repoPath, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "remote", "add", "origin", "git@github.com:example/wt-man.git"); err != nil {
		t.Fatal(err)
	}
	owner, name, ok := githubRepository(ctx, repoPath)
	if !ok || owner != "example" || name != "wt-man" {
		t.Fatalf("unexpected GitHub repository: %q/%q, %v", owner, name, ok)
	}
}

func TestDiscoverAndRemoveExistingAndMissingWorktrees(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	existingPath := filepath.Join(root, "existing")
	missingPath := filepath.Join(root, "missing")
	if err := os.Mkdir(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", "existing", existingPath); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, existingPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", "missing", missingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missingPath); err != nil {
		t.Fatal(err)
	}

	repositories, _, err := discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || len(repositories[0].Worktrees) != 2 {
		t.Fatalf("unexpected discovery result: %#v", repositories)
	}
	if repositories[0].MergeTarget != "main" {
		t.Fatalf("got merge target %q, want main", repositories[0].MergeTarget)
	}
	for _, item := range repositories[0].Worktrees {
		if item.Path == missingPath && !item.Missing {
			t.Fatal("missing worktree was not marked missing")
		}
		if !item.MergeKnown || item.Merged != (item.Branch == "missing") {
			t.Fatalf("unexpected merged status: %#v", item)
		}
		result := removeWorktree(ctx, repositories[0], item, false)
		if result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	if _, err := os.Stat(existingPath); !os.IsNotExist(err) {
		t.Fatalf("existing worktree directory still exists: %v", err)
	}
	output, err := git(ctx, repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, existingPath) || strings.Contains(output, missingPath) {
		t.Fatalf("worktree records still exist: %s", output)
	}
}

func TestModificationTimeFindsNewestFilesystemEntry(t *testing.T) {
	root := t.TempDir()
	olderPath := filepath.Join(root, "older")
	newerPath := filepath.Join(root, "nested", "newer")
	if err := os.MkdirAll(filepath.Dir(newerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(olderPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newerPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	older := time.Date(2025, 1, 2, 3, 4, 5, 0, time.Local)
	newer := older.Add(time.Hour)
	if err := os.Chtimes(olderPath, older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newerPath, newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(newerPath), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, older, older); err != nil {
		t.Fatal(err)
	}

	if got := modificationTime(root); !got.Equal(newer) {
		t.Fatalf("got %s, want %s", got, newer)
	}
}

func TestModelFiltersAndSelectsVisibleRows(t *testing.T) {
	repositories := []repository{{
		Name: "example",
		Worktrees: []worktree{
			{Path: "/tmp/one", Branch: "feature/one"},
			{Path: "/tmp/two", Branch: "fix/two"},
		},
	}}
	m := newModel(repositories)
	m.query = "fix"
	m.applyFilter()
	if len(m.visible) != 1 || m.item(m.visible[0]).Path != "/tmp/two" {
		t.Fatalf("unexpected filter results: %#v", m.visible)
	}
	m.toggleAllVisible()
	if !m.selected["/tmp/two"] || m.selected["/tmp/one"] {
		t.Fatalf("unexpected selection: %#v", m.selected)
	}
}

func TestModelUsesFullRepositoryNameWidth(t *testing.T) {
	m := newModel([]repository{
		{Name: "short", Worktrees: []worktree{{Path: "/tmp/one"}}},
		{Name: "a-much-longer-repository", Worktrees: []worktree{{Path: "/tmp/two"}}},
	})
	if m.repositoryWidth != len("a-much-longer-repository") {
		t.Fatalf("got repository width %d", m.repositoryWidth)
	}
	if !strings.Contains(m.browseView(), "a-much-longer-repository") {
		t.Fatal("full repository name was not rendered")
	}
}

func TestModelUsesFullBranchWidthBeforePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	branch := "feature/a-complete-descriptive-branch-name"
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{{Path: "/tmp/worktree-path", Branch: branch}},
	}})
	m.width = 100

	if m.branchWidth != len(branch) {
		t.Fatalf("got branch width %d, want %d", m.branchWidth, len(branch))
	}
	if !strings.Contains(m.browseView(), branch) {
		t.Fatal("full branch name was not rendered")
	}
	if m.pathColumnWidth() != 0 {
		t.Fatal("path column should give way to the full branch on a narrow terminal")
	}

	m.width = 140
	if m.pathColumnWidth() == 0 {
		t.Fatal("path column was not restored on a wide terminal")
	}

	m.width = 60
	if !m.compactRows() || !strings.Contains(m.browseView(), "Branch: "+branch) {
		t.Fatal("narrow layout did not move the full branch onto its own line")
	}
}

func TestModelShowsMergeTargetAndStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newModel([]repository{{
		Name:        "example",
		MergeTarget: "origin/main",
		Worktrees: []worktree{{
			Path:       "/tmp/merged",
			Branch:     "feature/merged",
			MergeKnown: true,
			Merged:     true,
		}},
	}})

	view := m.browseView()
	if !strings.Contains(view, "MERGED") || !strings.Contains(view, "Merged into origin/main: yes") {
		t.Fatalf("merge status was not rendered: %q", view)
	}
}

func TestModelWarnsWhenGitHubAuthenticationIsUnavailable(t *testing.T) {
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{{Path: "/tmp/example"}},
	}})
	m.githubAuthAvailable = false

	if view := m.browseView(); !strings.Contains(view, "Warning: GitHub authentication unavailable") {
		t.Fatalf("GitHub authentication warning was not rendered: %q", view)
	}
}

func TestModelShowsMissingWorktreeExplicitly(t *testing.T) {
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{{
			Path:     "/tmp/missing",
			Missing:  true,
			Prunable: true,
		}},
	}})

	view := m.browseView()
	if !strings.Contains(view, "missing") || !strings.Contains(view, "missing (prunable; Git record only)") {
		t.Fatalf("missing worktree state was not rendered: %q", view)
	}
}

func TestModelShowsModificationScanProgressAndResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{{Path: "/tmp/example"}},
	}})
	if !strings.Contains(m.browseView(), "scanning") {
		t.Fatal("modification scan progress was not rendered")
	}

	modified := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	updated, _ := m.Update(modificationTimeMsg{row: m.rows[0], modifiedAt: modified})
	m = updated.(model)
	if !strings.Contains(m.browseView(), "2026-08-24") {
		t.Fatal("modification date was not rendered")
	}
}

func TestModelUsesModificationCacheAndQueuesRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := "/tmp/cached-worktree"
	modified := time.Date(2026, 8, 23, 12, 0, 0, 0, time.Local)
	writeModificationCacheEntry(path, modified)

	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{{Path: path}},
	}})
	if got := m.item(m.rows[0]).ModifiedAt; !got.Equal(modified) {
		t.Fatalf("got cached modification time %s, want %s", got, modified)
	}
	if len(m.modificationQueue) != 0 {
		t.Fatalf("fresh cache entry was queued for scanning: %#v", m.modificationQueue)
	}

	updated, command := m.updateKey("r")
	m = updated.(model)
	if command == nil || len(m.modificationQueue) != 1 || !m.item(m.rows[0]).ModifiedAt.IsZero() {
		t.Fatal("forced refresh was not queued")
	}
}

func TestEnterAfterDeletionReturnsToUpdatedList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{
			{Path: "/tmp/deleted"},
			{Path: "/tmp/failed"},
		},
	}})
	m.screen = resultsScreen
	m.results = []deletionResult{
		{Path: "/tmp/deleted", Removed: true},
		{Path: "/tmp/failed", Err: errors.New("failed")},
	}

	updated, _ := m.updateKey("enter")
	m = updated.(model)
	if m.screen != browseScreen || len(m.rows) != 1 || m.item(m.rows[0]).Path != "/tmp/failed" {
		t.Fatalf("unexpected refreshed list: %#v", m.repositories)
	}

	updated, _ = m.Update(modificationTimeMsg{generation: 0, row: row{repository: 99, worktree: 99}})
	if updated.(model).generation != 1 {
		t.Fatal("stale modification scan changed the refreshed model")
	}
}

func TestDeletionWaitsForScanAndReportsEachWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{
			{Path: "/tmp/one"},
			{Path: "/tmp/two"},
		},
	}})
	m.selected["/tmp/one"] = true
	m.selected["/tmp/two"] = true
	m.screen = reviewScreen

	updated, command := m.updateKey("D")
	m = updated.(model)
	if command != nil || !m.deletionWaiting || !strings.Contains(m.deletingView(), "Finishing the active date scan") {
		t.Fatal("deletion did not wait visibly for the active date scan")
	}

	scan := m.modificationQueue[0]
	updated, command = m.Update(modificationTimeMsg{generation: m.generation, row: scan})
	m = updated.(model)
	if command == nil || m.deletionWaiting || len(m.deletionQueue) != 2 {
		t.Fatal("first sequential deletion was not started")
	}

	updated, command = m.Update(deletionProgressMsg{result: deletionResult{Path: "/tmp/one", Removed: true}})
	m = updated.(model)
	if command == nil || len(m.deletionQueue) != 1 || !strings.Contains(m.deletingView(), "1/2") {
		t.Fatal("second sequential deletion was not started with updated progress")
	}

	updated, command = m.Update(deletionProgressMsg{result: deletionResult{Path: "/tmp/two", Removed: true}})
	m = updated.(model)
	if command != nil || m.screen != resultsScreen || len(m.results) != 2 {
		t.Fatal("sequential deletion did not finish on the results screen")
	}
}

func TestModelFiltersByUnarchivedSessions(t *testing.T) {
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{
			{Path: "/tmp/open", Sessions: sessionCounts{Claude: 1}},
			{Path: "/tmp/archived"},
		},
	}})

	updated, _ := m.updateKey("u")
	m = updated.(model)
	if len(m.visible) != 1 || m.item(m.visible[0]).Path != "/tmp/open" {
		t.Fatalf("unexpected with-unarchived results: %#v", m.visible)
	}

	updated, _ = m.updateKey("u")
	m = updated.(model)
	if len(m.visible) != 1 || m.item(m.visible[0]).Path != "/tmp/archived" {
		t.Fatalf("unexpected without-unarchived results: %#v", m.visible)
	}

	updated, _ = m.updateKey("u")
	m = updated.(model)
	if len(m.visible) != 2 {
		t.Fatalf("got %d results after cycling to all", len(m.visible))
	}
}

func TestClaudeSessionJSONFields(t *testing.T) {
	data := []byte(`{"sessionId":"abc","cwd":"/tmp/example","isArchived":false}`)
	var session struct {
		SessionID  string
		CWD        string
		IsArchived *bool
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "abc" || session.CWD != "/tmp/example" || session.IsArchived == nil || *session.IsArchived {
		t.Fatalf("unexpected session: %#v", session)
	}
}
