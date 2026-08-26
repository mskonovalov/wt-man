package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charlievieth/fastwalk"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
)

type sessionCounts struct {
	Claude      int
	Codex       int
	ClaudeKnown bool
	CodexKnown  bool
}

type modificationCacheEntry struct {
	ModifiedAt time.Time `json:"modified_at"`
	ScannedAt  time.Time `json:"scanned_at"`
}

const modificationCacheTTL = 24 * time.Hour

type worktree struct {
	Path        string
	Branch      string
	Head        string
	Detached    bool
	Locked      bool
	LockReason  string
	Prunable    bool
	Bare        bool
	Missing     bool
	Merged      bool
	MergeKnown  bool
	MergeSource string
	CreatedAt   time.Time
	ModifiedAt  time.Time
	Sessions    sessionCounts
}

type repository struct {
	Name        string
	MainPath    string
	MergeTarget string
	Worktrees   []worktree
}

var ignoredDirectories = map[string]bool{
	".cache": true, ".yarn": true, "node_modules": true, "vendor": true,
}

func discover(ctx context.Context, root string) ([]repository, bool, error) {
	root, err := canonicalPath(root)
	if err != nil {
		return nil, false, fmt.Errorf("resolve root: %w", err)
	}

	var gitRoots []string
	var claude map[string]map[string]bool
	var codex map[string]int
	var claudeKnown bool
	var codexKnown bool
	var rootsErr error
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		gitRoots, rootsErr = findGitRoots(root)
	}()
	go func() {
		defer wait.Done()
		claude, claudeKnown = readClaudeSessions()
	}()
	go func() {
		defer wait.Done()
		codex, codexKnown = readCodexSessions(ctx)
	}()
	wait.Wait()
	if rootsErr != nil {
		return nil, false, rootsErr
	}

	seen := make(map[string]bool)
	primaryPaths := findPrimaryWorktreePaths(ctx, gitRoots)
	var repositories []repository
	for _, gitRoot := range gitRoots {
		commonDirectory, err := git(ctx, gitRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		commonDirectory, err = canonicalPath(commonDirectory)
		if err != nil {
			continue
		}
		if seen[commonDirectory] {
			continue
		}
		seen[commonDirectory] = true

		output, err := git(ctx, gitRoot, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		items, err := parseWorktrees(output)
		if err != nil || len(items) == 0 {
			continue
		}
		primaryPath, err := primaryWorktreePath(commonDirectory, items, primaryPaths)
		if err != nil {
			continue
		}
		merged, mergeTarget := mergedBranches(ctx, primaryPath)

		var linked []worktree
		for index, item := range items {
			item.Path, _ = canonicalPath(item.Path)
			if index == 0 {
				continue
			}
			_, statErr := os.Stat(item.Path)
			item.Missing = errors.Is(statErr, fs.ErrNotExist)
			item.CreatedAt = creationTime(item.Path)
			if item.Branch != "" && mergeTarget != "" {
				item.Merged = merged[item.Branch]
				item.MergeKnown = true
				item.MergeSource = "Git"
			}
			linked = append(linked, item)
		}
		if len(linked) == 0 {
			continue
		}
		sort.SliceStable(linked, func(i, j int) bool {
			left, right := linked[i].CreatedAt, linked[j].CreatedAt
			if left.IsZero() {
				return false
			}
			if right.IsZero() {
				return true
			}
			return left.Before(right)
		})
		repositories = append(repositories, repository{
			Name: filepath.Base(primaryPath), MainPath: primaryPath, MergeTarget: mergeTarget, Worktrees: linked,
		})
	}
	sort.Slice(repositories, func(i, j int) bool {
		return repositories[i].Name < repositories[j].Name
	})
	assignSessions(repositories, claude, codex, claudeKnown, codexKnown)
	githubAuthenticated := overlayGitHubMerged(ctx, repositories)
	return repositories, githubAuthenticated, nil
}

func findPrimaryWorktreePaths(ctx context.Context, gitRoots []string) map[string]string {
	result := make(map[string]string)
	for _, root := range gitRoots {
		commonDirectory, err := git(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		commonDirectory, err = canonicalPath(commonDirectory)
		if err != nil {
			continue
		}
		gitDirectory, err := git(ctx, root, "rev-parse", "--absolute-git-dir")
		if err != nil {
			continue
		}
		gitDirectory, err = canonicalPath(gitDirectory)
		if err != nil || gitDirectory != commonDirectory {
			continue
		}
		topLevel, err := git(ctx, root, "rev-parse", "--show-toplevel")
		if err != nil {
			continue
		}
		if topLevel, err = canonicalPath(topLevel); err == nil {
			result[commonDirectory] = topLevel
		}
	}
	return result
}

func primaryWorktreePath(commonDirectory string, items []worktree, primaryPaths map[string]string) (string, error) {
	if items[0].Bare {
		return canonicalPath(items[0].Path)
	}
	if primaryPath := primaryPaths[commonDirectory]; primaryPath != "" {
		return primaryPath, nil
	}
	return canonicalPath(items[0].Path)
}

func mergedBranches(ctx context.Context, directory string) (map[string]bool, string) {
	type candidate struct {
		ref     string
		display string
	}
	var candidates []candidate
	if symbolic, err := git(ctx, directory, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		candidates = append(candidates, candidate{ref: symbolic, display: strings.TrimPrefix(symbolic, "refs/remotes/")})
	}
	candidates = append(candidates,
		candidate{ref: "refs/remotes/origin/main", display: "origin/main"},
		candidate{ref: "refs/remotes/origin/master", display: "origin/master"},
		candidate{ref: "refs/heads/main", display: "main"},
		candidate{ref: "refs/heads/master", display: "master"},
	)
	var target candidate
	for _, current := range candidates {
		if _, err := git(ctx, directory, "rev-parse", "--verify", "--quiet", current.ref+"^{commit}"); err == nil {
			target = current
			break
		}
	}
	merged := make(map[string]bool)
	if target.ref == "" {
		return merged, ""
	}
	output, err := git(ctx, directory, "for-each-ref", "--format=%(refname)", "--merged="+target.ref, "refs/heads")
	if err != nil {
		return merged, ""
	}
	for _, branch := range strings.Split(output, "\n") {
		if strings.HasPrefix(branch, "refs/heads/") {
			merged[strings.TrimPrefix(branch, "refs/heads/")] = true
		}
	}
	return merged, target.display
}

func assignSessions(repositories []repository, claude map[string]map[string]bool, codex map[string]int, claudeKnown, codexKnown bool) {
	for repositoryIndex := range repositories {
		for worktreeIndex := range repositories[repositoryIndex].Worktrees {
			repositories[repositoryIndex].Worktrees[worktreeIndex].Sessions.ClaudeKnown = claudeKnown
			repositories[repositoryIndex].Worktrees[worktreeIndex].Sessions.CodexKnown = codexKnown
		}
	}
	for cwd, sessions := range claude {
		if repositoryIndex, worktreeIndex, ok := containingWorktree(repositories, cwd); ok {
			repositories[repositoryIndex].Worktrees[worktreeIndex].Sessions.Claude += len(sessions)
		}
	}
	for cwd, count := range codex {
		if repositoryIndex, worktreeIndex, ok := containingWorktree(repositories, cwd); ok {
			repositories[repositoryIndex].Worktrees[worktreeIndex].Sessions.Codex += count
		}
	}
}

func containingWorktree(repositories []repository, path string) (int, int, bool) {
	bestRepository, bestWorktree, bestLength := 0, 0, -1
	for repositoryIndex, repo := range repositories {
		for worktreeIndex, item := range repo.Worktrees {
			relative, err := filepath.Rel(item.Path, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			if len(item.Path) > bestLength {
				bestRepository, bestWorktree, bestLength = repositoryIndex, worktreeIndex, len(item.Path)
			}
		}
	}
	return bestRepository, bestWorktree, bestLength >= 0
}

func overlayGitHubMerged(ctx context.Context, repositories []repository) bool {
	type repositoryQuery struct {
		rows map[string]row
	}
	queries := make(map[string]repositoryQuery)
	var query strings.Builder
	query.WriteString("query {")
	for repositoryIndex, repo := range repositories {
		owner, name, ok := githubRepository(ctx, repo.MainPath)
		if !ok {
			continue
		}
		repositoryAlias := fmt.Sprintf("r%d", repositoryIndex)
		current := repositoryQuery{rows: make(map[string]row)}
		var commitsQuery strings.Builder
		for worktreeIndex, item := range repo.Worktrees {
			if item.Branch == "" || item.Head == "" {
				continue
			}
			commitAlias := fmt.Sprintf("c%d", worktreeIndex)
			current.rows[commitAlias] = row{repository: repositoryIndex, worktree: worktreeIndex}
			fmt.Fprintf(&commitsQuery, "%s: object(oid:%q) { ... on Commit { associatedPullRequests(first:10) { nodes { mergedAt baseRefName headRefName headRefOid } } } }", commitAlias, item.Head)
		}
		if len(current.rows) > 0 {
			fmt.Fprintf(&query, "%s: repository(owner:%q,name:%q) {%s}", repositoryAlias, owner, name, commitsQuery.String())
			queries[repositoryAlias] = current
		}
	}
	query.WriteString("}")
	token, _ := auth.TokenForHost("github.com")
	if token == "" {
		return false
	}
	if len(queries) == 0 {
		return true
	}
	client, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: token})
	if err != nil {
		return true
	}

	apiContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response := make(map[string]json.RawMessage)
	if client.DoWithContext(apiContext, query.String(), nil, &response) != nil {
		return true
	}
	for repositoryAlias, repositoryQuery := range queries {
		var commits map[string]struct {
			AssociatedPullRequests struct {
				Nodes []struct {
					MergedAt    *time.Time `json:"mergedAt"`
					BaseRefName string     `json:"baseRefName"`
					HeadRefName string     `json:"headRefName"`
					HeadRefOID  string     `json:"headRefOid"`
				} `json:"nodes"`
			} `json:"associatedPullRequests"`
		}
		if json.Unmarshal(response[repositoryAlias], &commits) != nil {
			continue
		}
		for commitAlias, current := range repositoryQuery.rows {
			item := &repositories[current.repository].Worktrees[current.worktree]
			base := strings.TrimPrefix(repositories[current.repository].MergeTarget, "origin/")
			for _, pullRequest := range commits[commitAlias].AssociatedPullRequests.Nodes {
				if pullRequest.MergedAt != nil && pullRequest.BaseRefName == base && pullRequest.HeadRefName == item.Branch && pullRequest.HeadRefOID == item.Head {
					item.Merged = true
					item.MergeKnown = true
					item.MergeSource = "GitHub"
					break
				}
			}
		}
	}
	return true
}

func githubRepository(ctx context.Context, directory string) (string, string, bool) {
	remote, err := git(ctx, directory, "remote", "get-url", "origin")
	if err != nil {
		return "", "", false
	}
	var path string
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "http://github.com/", "ssh://git@github.com/"} {
		if strings.HasPrefix(remote, prefix) {
			path = strings.TrimPrefix(remote, prefix)
			break
		}
	}
	if path == "" {
		return "", "", false
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func findGitRoots(root string) ([]string, error) {
	var roots []string
	var visit func(string) error
	visit = func(directory string) error {
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			if isWorktreeGitRoot(directory) {
				roots = append(roots, directory)
				return nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if isStandaloneGitDirectory(directory) {
			roots = append(roots, directory)
			return nil
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() || ignoredDirectories[entry.Name()] {
				continue
			}
			if err := visit(filepath.Join(directory, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return roots, visit(root)
}

func isWorktreeGitRoot(directory string) bool {
	topLevel, err := git(context.Background(), directory, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	topLevel, err = canonicalPath(topLevel)
	if err != nil {
		return false
	}
	directory, err = canonicalPath(directory)
	return err == nil && topLevel == directory
}

func isStandaloneGitDirectory(directory string) bool {
	head, headErr := os.Stat(filepath.Join(directory, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(directory, "objects"))
	if headErr != nil || !head.Mode().IsRegular() || objectsErr != nil || !objects.IsDir() {
		return false
	}
	gitDirectory, err := git(context.Background(), directory, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	gitDirectory, err = canonicalPath(gitDirectory)
	if err != nil {
		return false
	}
	directory, err = canonicalPath(directory)
	return err == nil && gitDirectory == directory
}

func parseWorktrees(output string) ([]worktree, error) {
	var items []worktree
	var current *worktree
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if current != nil {
				items = append(items, *current)
				current = nil
			}
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			if current != nil {
				items = append(items, *current)
			}
			current = &worktree{Path: strings.TrimPrefix(line, "worktree ")}
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("invalid git worktree output: %q", line)
		}
		switch {
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "detached":
			current.Detached = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			current.Locked = true
			current.LockReason = strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			current.Prunable = true
		case line == "bare":
			current.Bare = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if current != nil {
		items = append(items, *current)
	}
	return items, nil
}

func readClaudeSessions() (map[string]map[string]bool, bool) {
	result := make(map[string]map[string]bool)
	home, err := os.UserHomeDir()
	if err != nil {
		return result, false
	}
	base := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	if _, err := os.Stat(base); err != nil {
		return result, false
	}
	available := true
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			available = false
			return nil
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "local_") || filepath.Ext(path) != ".json" {
			return nil
		}
		var session struct {
			SessionID  string
			CWD        string
			IsArchived *bool
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &session) != nil {
			available = false
			return nil
		}
		if session.IsArchived == nil {
			available = false
			return nil
		}
		if *session.IsArchived {
			return nil
		}
		if session.SessionID == "" || session.CWD == "" {
			available = false
			return nil
		}
		cwd, err := canonicalPath(session.CWD)
		if err != nil {
			available = false
			return nil
		}
		if result[cwd] == nil {
			result[cwd] = make(map[string]bool)
		}
		result[cwd][session.SessionID] = true
		return nil
	})
	return result, available && err == nil
}

func readCodexSessions(ctx context.Context) (map[string]int, bool) {
	result := make(map[string]int)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return result, false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return result, false
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	database := filepath.Join(codexHome, "sqlite", "state_5.sqlite")
	if _, err := os.Stat(database); err != nil {
		return result, false
	}
	output, err := exec.CommandContext(ctx, "sqlite3", "-separator", "\t", database,
		"SELECT cwd, COUNT(*) FROM threads WHERE archived = 0 GROUP BY cwd;").Output()
	if err != nil {
		return result, false
	}
	available := true
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return result, true
	}
	for _, line := range strings.Split(trimmed, "\n") {
		separator := strings.LastIndexByte(line, '\t')
		if separator == -1 {
			available = false
			continue
		}
		count, err := strconv.Atoi(line[separator+1:])
		if err != nil {
			available = false
			continue
		}
		cwd, err := canonicalPath(line[:separator])
		if err == nil {
			result[cwd] = count
		} else {
			available = false
		}
	}
	return result, available
}

func removeWorktree(ctx context.Context, repo repository, item worktree, deleteBranch bool) deletionResult {
	result := deletionResult{Path: item.Path, Branch: item.Branch, Missing: item.Missing}
	if item.Locked {
		result.Err = fmt.Errorf("worktree is locked; unlock it before deletion")
		return result
	}
	if _, err := git(ctx, repo.MainPath, "worktree", "remove", "--force", item.Path); err != nil {
		result.Err = err
		return result
	}
	result.Removed = true
	if deleteBranch && item.Branch != "" {
		if _, err := git(ctx, repo.MainPath, "branch", "-d", item.Branch); err != nil {
			result.Err = fmt.Errorf("worktree removed; branch not deleted: %w", err)
			return result
		}
		result.BranchDeleted = true
	}
	return result
}

func modificationTime(path string) time.Time {
	var latest time.Time
	var lock sync.Mutex
	_ = fastwalk.Walk(nil, path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		lock.Lock()
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		lock.Unlock()
		return nil
	})
	return latest
}

func readModificationCache() map[string]modificationCacheEntry {
	cache := make(map[string]modificationCacheEntry)
	directory, err := os.UserCacheDir()
	if err != nil {
		return cache
	}
	data, err := os.ReadFile(filepath.Join(directory, "wt-man", "modtimes.json"))
	if err != nil {
		return cache
	}
	_ = json.Unmarshal(data, &cache)
	return cache
}

func writeModificationCacheEntry(path string, modifiedAt time.Time) {
	directory, err := os.UserCacheDir()
	if err != nil {
		return
	}
	directory = filepath.Join(directory, "wt-man")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return
	}
	cache := readModificationCache()
	cache[path] = modificationCacheEntry{ModifiedAt: modifiedAt, ScannedAt: time.Now()}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	temporary, err := os.CreateTemp(directory, "modtimes-*.json")
	if err != nil {
		return
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, "modtimes.json")); err != nil {
		_ = os.Remove(temporaryPath)
	}
}

func git(ctx context.Context, directory string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return real, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return filepath.Clean(absolute), nil
	}
	return "", err
}
