package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func safeWorktree(path string) worktree {
	return worktree{
		Path:           path,
		Branch:         "feature/safe",
		MergeChecked:   true,
		MergeKnown:     true,
		Merged:         true,
		ChangesChecked: true,
		ChangesKnown:   true,
		Sessions:       sessionCounts{ClaudeKnown: true, CodexKnown: true},
	}
}

func TestParseWorktrees(t *testing.T) {
	output := "worktree /tmp/repo\n" +
		"HEAD abc123\n" +
		"branch refs/heads/main\n" +
		"bare\n\n" +
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
	if !items[0].Bare {
		t.Fatalf("bare worktree marker was not parsed: %#v", items[0])
	}
	if items[1].Head != "def456" || items[1].Branch != "feature/test" || !items[1].Locked || items[1].LockReason != "reason" {
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

	repositories, err := discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || len(repositories[0].Worktrees) != 2 {
		t.Fatalf("unexpected discovery result: %#v", repositories)
	}
	if repositories[0].MergeTarget != "" || repositories[0].Worktrees[0].MergeKnown {
		t.Fatalf("merge status blocked initial discovery: %#v", repositories[0])
	}
	m := newModel(repositories)
	message := scanMergeStatus(m.generation, 0, repositories[0].MainPath)()
	updated, command := m.Update(message)
	m = updated.(model)
	if command == nil || m.repositories[0].MergeTarget != "main" {
		t.Fatalf("background merge scan did not finish correctly: %#v", m.repositories[0])
	}
	repositories = m.repositories
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

func TestDiscoverUsesBareRepositoryAsPrimaryPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedPath := filepath.Join(root, "seed")
	barePath := filepath.Join(root, "example.git")
	linkedPath := filepath.Join(root, "linked")
	if err := os.Mkdir(seedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, seedPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, seedPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "clone", "--bare", seedPath, barePath); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, barePath, "worktree", "add", "-b", "linked", linkedPath, "main"); err != nil {
		t.Fatal(err)
	}

	repositories, err := discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	barePath, _ = canonicalPath(barePath)
	linkedPath, _ = canonicalPath(linkedPath)
	if len(repositories) != 1 || repositories[0].MainPath != barePath || repositories[0].Name != "example.git" {
		t.Fatalf("bare repository identity was not preserved: %#v", repositories)
	}
	if len(repositories[0].Worktrees) != 1 || repositories[0].Worktrees[0].Path != linkedPath {
		t.Fatalf("bare repository worktree was not discovered: %#v", repositories)
	}
}

func TestDiscoverUsesWorktreePathWithSeparateGitDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitDirectory := filepath.Join(root, "metadata")
	primaryPath := filepath.Join(root, "primary")
	linkedPath := filepath.Join(root, "linked")
	if _, err := git(ctx, root, "init", "-b", "main", "--separate-git-dir", gitDirectory, primaryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, primaryPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, primaryPath, "worktree", "add", "-b", "linked", linkedPath); err != nil {
		t.Fatal(err)
	}
	repositories, err := discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	primaryPath, _ = canonicalPath(primaryPath)
	if len(repositories) != 1 || repositories[0].MainPath != primaryPath || repositories[0].Name != "primary" {
		t.Fatalf("separate Git directory changed repository identity: %#v", repositories)
	}
}

func TestRemoveWorktreeKeepsUnmergedBranch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	linkedPath := filepath.Join(root, "linked")
	if err := os.Mkdir(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "worktree", "add", "-b", "unmerged", linkedPath); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, linkedPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "unmerged"); err != nil {
		t.Fatal(err)
	}

	result := removeWorktree(ctx, repository{MainPath: repoPath}, worktree{Path: linkedPath, Branch: "unmerged"}, true)
	if result.Err == nil || !result.Removed || result.BranchDeleted {
		t.Fatalf("unexpected safe branch deletion result: %#v", result)
	}
	if _, err := git(ctx, repoPath, "show-ref", "--verify", "refs/heads/unmerged"); err != nil {
		t.Fatalf("unmerged branch was not preserved: %v", err)
	}
}

func TestMergedBranchesUsesFullyQualifiedFallbackRefs(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "switch", "--orphan", "tag-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "unrelated tag target"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "update-ref", "refs/tags/main", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "switch", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "branch", "-D", "tag-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/missing"); err != nil {
		t.Fatal(err)
	}
	merged, target := mergedBranches(ctx, repoPath)
	if target != "main" || !merged["main"] {
		t.Fatalf("fully qualified fallback was not used: target=%q merged=%#v", target, merged)
	}
}

func TestGitDirectoryHeuristicDoesNotPruneOrdinarySubtree(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "ordinary")
	nested := filepath.Join(container, "nested")
	if err := os.MkdirAll(filepath.Join(container, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "HEAD"), []byte("not Git"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), container, "init", "-b", "main", nested); err != nil {
		t.Fatal(err)
	}
	roots, err := findGitRoots(root)
	if err != nil {
		t.Fatal(err)
	}
	nested, _ = canonicalPath(nested)
	if len(roots) == 1 {
		roots[0], _ = canonicalPath(roots[0])
	}
	if len(roots) != 1 || roots[0] != nested {
		t.Fatalf("ordinary directory pruned nested repository: %#v", roots)
	}
}

func TestBrokenDotGitDoesNotPruneNestedRepository(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "ordinary")
	nested := filepath.Join(container, "nested")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, ".git"), []byte("gitdir: /does/not/exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), container, "init", "-b", "main", nested); err != nil {
		t.Fatal(err)
	}
	roots, err := findGitRoots(root)
	if err != nil {
		t.Fatal(err)
	}
	nested, _ = canonicalPath(nested)
	if len(roots) == 1 {
		roots[0], _ = canonicalPath(roots[0])
	}
	if len(roots) != 1 || roots[0] != nested {
		t.Fatalf("broken .git entry pruned nested repository: %#v", roots)
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

func TestWorktreeChangesIncludesUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if changed, known := worktreeChanges(ctx, repoPath); !known || changed {
		t.Fatalf("clean worktree returned changed=%v known=%v", changed, known)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "untracked.txt"), []byte("local work"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, known := worktreeChanges(ctx, repoPath); !known || !changed {
		t.Fatalf("untracked file returned changed=%v known=%v", changed, known)
	}
}

func TestWorktreeChangesIgnoresIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	if _, err := git(ctx, repoPath, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, repoPath, "-c", "user.name=wt-man", "-c", "user.email=wt-man@example.com", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "ignored.txt"), []byte("generated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, known := worktreeChanges(ctx, repoPath); !known || changed {
		t.Fatalf("ignored file returned changed=%v known=%v", changed, known)
	}
}

func TestWorktreeChangesReportsUnknownForBrokenWorktree(t *testing.T) {
	if changed, known := worktreeChanges(context.Background(), t.TempDir()); known || changed {
		t.Fatalf("non-repository returned changed=%v known=%v", changed, known)
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

func TestModelFiltersBySafety(t *testing.T) {
	safe := safeWorktree("/tmp/safe")
	notSafe := safeWorktree("/tmp/not-safe")
	notSafe.HasChanges = true
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{
			safe,
			notSafe,
			{Path: "/tmp/checking"},
		},
	}})
	m.githubAuthChecked = true
	want := []string{"/tmp/safe", "/tmp/not-safe", "/tmp/checking"}
	for index, path := range want {
		updated, _ := m.updateKey("s")
		m = updated.(model)
		if len(m.visible) != 1 || m.item(m.visible[0]).Path != path {
			t.Fatalf("cycle %d returned %#v, want %s", index+1, m.visible, path)
		}
	}
	updated, _ := m.updateKey("s")
	m = updated.(model)
	if len(m.visible) != 3 {
		t.Fatalf("all safety statuses returned %d rows, want 3", len(m.visible))
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
	item := safeWorktree("/tmp/merged")
	item.Branch = "feature/merged"
	m := newModel([]repository{{
		Name:        "example",
		MergeTarget: "origin/main",
		Worktrees:   []worktree{item},
	}})

	view := m.browseView()
	if !strings.Contains(view, "SAFE") || !strings.Contains(view, "Safety: SAFE") || !strings.Contains(view, "Merged into origin/main: yes") {
		t.Fatalf("aggregate safety was not rendered: %q", view)
	}
}

func TestGitHubMergeStatusUpdatesRows(t *testing.T) {
	m := newModel([]repository{{
		Name: "example",
		Worktrees: []worktree{{
			Path:   "/tmp/merged",
			Branch: "feature/merged",
		}},
	}})
	m.githubMergePending = true

	updated, _ := m.Update(githubMergeStatusMsg{
		generation:    m.generation,
		authenticated: true,
		merged:        []row{{repository: 0, worktree: 0}},
	})
	m = updated.(model)
	item := m.item(m.rows[0])
	if m.githubMergePending || !m.githubAuthChecked || !m.githubAuthAvailable || !item.MergeChecked || !item.Merged || item.MergeSource != "GitHub" {
		t.Fatalf("GitHub merge status was not applied: %#v, model=%#v", item, m)
	}
}

func TestModelTreatsUnavailableMergeEvidenceAsNotSafe(t *testing.T) {
	item := safeWorktree("/tmp/example")
	item.Merged = false
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{item},
	}})
	m.githubMergePending = false
	m.githubAuthChecked = true
	m.githubAuthAvailable = false

	if view := m.browseView(); !strings.Contains(view, "Safety: NO") || !strings.Contains(view, "committed work is not in the default branch") {
		t.Fatalf("missing merge evidence was not treated as unsafe: %q", view)
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
	if !strings.Contains(view, "missing") || !strings.Contains(view, "missing (Git record only); prunable") {
		t.Fatalf("missing worktree state was not rendered: %q", view)
	}
}

func TestModelDoesNotCallEveryMissingWorktreePrunable(t *testing.T) {
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{{Path: "/tmp/missing", Missing: true}},
	}})
	view := m.browseView()
	if !strings.Contains(view, "missing (Git record only)") || strings.Contains(view, "prunable") {
		t.Fatalf("missing non-prunable worktree state was rendered incorrectly: %q", view)
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
	view := m.browseView()
	if !strings.Contains(view, "2026-08-24") {
		t.Fatal("modification date was not rendered")
	}
	if strings.Contains(view, "Date scan") {
		t.Fatalf("completed date scan remained visible: %q", view)
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
	one := safeWorktree("/tmp/one")
	two := safeWorktree("/tmp/two")
	m := newModel([]repository{{
		Name:      "example",
		Worktrees: []worktree{one, two},
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

func TestDeletionWaitsForSafetyChecks(t *testing.T) {
	m := newModel([]repository{{Name: "example", Worktrees: []worktree{{Path: "/tmp/checking"}}}})
	m.selected["/tmp/checking"] = true
	m.screen = reviewScreen

	updated, command := m.updateKey("D")
	m = updated.(model)
	if command != nil || m.screen != reviewScreen || !strings.Contains(m.reviewView(), "Waiting for safety checks") {
		t.Fatal("deletion did not wait for safety checks")
	}
}

func TestSafetyRejectsSessionsAndUnknownProviders(t *testing.T) {
	active := safeWorktree("/tmp/active")
	active.Sessions.Claude = 1
	unknown := safeWorktree("/tmp/unknown")
	unknown.Sessions.CodexKnown = false
	m := newModel([]repository{{Name: "example", Worktrees: []worktree{active, unknown}}})
	m.githubAuthChecked = true

	for _, current := range m.rows {
		state, reasons := m.safety(current)
		if state != safetyNotSafe || len(reasons) == 0 {
			t.Fatalf("session risk was not unsafe: %s state=%v reasons=%v", m.item(current).Path, state, reasons)
		}
	}
}

func TestReadClaudeSessionsFromFixture(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "worktree", "nested")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"sessionId":"abc","cwd":"` + cwd + `","isArchived":false}`)
	if err := os.WriteFile(filepath.Join(base, "local_test.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, known := readClaudeSessions()
	cwd, _ = canonicalPath(cwd)
	if !known || len(sessions[cwd]) != 1 || !sessions[cwd]["abc"] {
		t.Fatalf("unexpected sessions: %#v, known=%v", sessions, known)
	}
	if err := os.WriteFile(filepath.Join(base, "local_broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, known = readClaudeSessions()
	if known {
		t.Fatal("malformed Claude session source was reported as known")
	}
	if err := os.Remove(filepath.Join(base, "local_broken.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "local_incomplete.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, known = readClaudeSessions()
	if known {
		t.Fatal("structurally incomplete Claude session source was reported as known")
	}
}

func TestAssignSessionsUsesDeepestContainingWorktree(t *testing.T) {
	repositories := []repository{{
		Name: "example",
		Worktrees: []worktree{
			{Path: "/tmp/repo"},
			{Path: "/tmp/repo/nested"},
		},
	}}
	claude := map[string]map[string]bool{"/tmp/repo/nested/subdirectory": {"session": true}}
	codex := map[string]int{"/tmp/repo/nested/another": 2}
	assignSessions(repositories, claude, codex, true, true)
	if repositories[0].Worktrees[0].Sessions.Claude != 0 || repositories[0].Worktrees[0].Sessions.Codex != 0 {
		t.Fatalf("outer worktree received nested sessions: %#v", repositories)
	}
	got := repositories[0].Worktrees[1].Sessions
	if got.Claude != 1 || got.Codex != 2 || !got.ClaudeKnown || !got.CodexKnown {
		t.Fatalf("nested worktree did not receive sessions: %#v", got)
	}
}

func TestLockedWorktreeIsRefusedAndExplained(t *testing.T) {
	item := safeWorktree("/tmp/locked")
	item.Locked = true
	item.LockReason = "in use"
	result := removeWorktree(context.Background(), repository{MainPath: "/does/not/matter"}, item, false)
	if result.Err == nil || result.Removed || !strings.Contains(result.Err.Error(), "locked") {
		t.Fatalf("unexpected locked deletion result: %#v", result)
	}
	m := newModel([]repository{{Name: "example", Worktrees: []worktree{item}}})
	m.selected[item.Path] = true
	m.screen = reviewScreen
	view := m.reviewView()
	if !strings.Contains(view, "NOT SAFE: worktree is locked") || !strings.Contains(view, "will not delete (in use)") || strings.Contains(view, "delete files and Git record") {
		t.Fatalf("locked state was not explained: %q", view)
	}
}

func TestReviewSummarizesSessionRisk(t *testing.T) {
	item := safeWorktree("/tmp/session")
	item.Sessions = sessionCounts{Claude: 2, ClaudeKnown: true}
	m := newModel([]repository{{Name: "example", Worktrees: []worktree{item}}})
	m.githubAuthChecked = true
	m.selected[item.Path] = true
	view := m.reviewView()
	if !strings.Contains(view, "NOT SAFE: session status is unknown; has unarchived sessions") || strings.Contains(view, "Claude 2") || strings.Contains(view, "Codex 0") {
		t.Fatalf("session risk was not summarized: %q", view)
	}
}

func TestBrowseHeaderIsBoundedToTerminalWidth(t *testing.T) {
	m := newModel([]repository{{Name: strings.Repeat("repository", 20), Worktrees: []worktree{{Path: "/tmp/one"}}}})
	m.width = 40
	for _, line := range strings.Split(m.browseView(), "\n") {
		if strings.Contains(line, "REPOSITORY") && len([]rune(line)) > m.width {
			t.Fatalf("header width %d exceeds terminal width %d: %q", len([]rune(line)), m.width, line)
		}
	}
}

func TestCtrlCDoesNotInterruptActiveDeletion(t *testing.T) {
	m := newModel(nil)
	m.screen = deletingScreen
	updated, command := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if command != nil || updated.(model).screen != deletingScreen {
		t.Fatal("Ctrl+C interrupted active deletion")
	}
}

func TestCompactPageSizeAccountsForWrappedBranches(t *testing.T) {
	branch := strings.Repeat("very-long-branch/", 12)
	m := newModel([]repository{{Name: "example", Worktrees: []worktree{{Path: "/tmp/one", Branch: branch}}}})
	m.width = 40
	m.height = 24
	if m.compactRowHeight() < 4 || m.pageSize() > 3 {
		t.Fatalf("wrapped branch was not reflected in pagination: height=%d page=%d", m.compactRowHeight(), m.pageSize())
	}
}
