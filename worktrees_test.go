package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if items[1].Branch != "feature/test" || !items[1].Locked {
		t.Fatalf("unexpected linked worktree: %#v", items[1])
	}
	if !items[2].Detached || !items[2].Prunable {
		t.Fatalf("unexpected detached worktree: %#v", items[2])
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
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", "missing", missingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missingPath); err != nil {
		t.Fatal(err)
	}

	repositories, err := discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || len(repositories[0].Worktrees) != 2 {
		t.Fatalf("unexpected discovery result: %#v", repositories)
	}
	for _, item := range repositories[0].Worktrees {
		if item.Path == missingPath && !item.Missing {
			t.Fatal("missing worktree was not marked missing")
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
