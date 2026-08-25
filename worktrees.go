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
)

type sessionCounts struct {
	Claude int
	Codex  int
}

type worktree struct {
	Path      string
	Branch    string
	Detached  bool
	Locked    bool
	Prunable  bool
	Missing   bool
	CreatedAt time.Time
	Sessions  sessionCounts
}

type repository struct {
	Name      string
	MainPath  string
	Worktrees []worktree
}

var ignoredDirectories = map[string]bool{
	".cache": true, ".yarn": true, "node_modules": true, "vendor": true,
}

func discover(ctx context.Context, root string) ([]repository, error) {
	root, err := canonicalPath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}

	var gitRoots []string
	var claude map[string]map[string]bool
	var codex map[string]int
	var rootsErr error
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		gitRoots, rootsErr = findGitRoots(root)
	}()
	go func() {
		defer wait.Done()
		claude = readClaudeSessions()
	}()
	go func() {
		defer wait.Done()
		codex = readCodexSessions(ctx)
	}()
	wait.Wait()
	if rootsErr != nil {
		return nil, rootsErr
	}

	seen := make(map[string]bool)
	var repositories []repository
	for _, gitRoot := range gitRoots {
		commonDirectory, err := git(ctx, gitRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		commonDirectory = filepath.Clean(commonDirectory)
		if seen[commonDirectory] {
			continue
		}
		seen[commonDirectory] = true

		output, err := git(ctx, gitRoot, "worktree", "list", "--porcelain")
		if err != nil {
			return nil, err
		}
		mainPath, err := canonicalPath(filepath.Dir(commonDirectory))
		if err != nil {
			continue
		}
		items, err := parseWorktrees(output)
		if err != nil {
			return nil, err
		}

		var linked []worktree
		for _, item := range items {
			item.Path, _ = canonicalPath(item.Path)
			if item.Path == mainPath {
				continue
			}
			_, statErr := os.Stat(item.Path)
			item.Missing = errors.Is(statErr, fs.ErrNotExist)
			item.CreatedAt = creationTime(item.Path)
			item.Sessions.Claude = len(claude[item.Path])
			item.Sessions.Codex = codex[item.Path]
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
			Name: filepath.Base(mainPath), MainPath: mainPath, Worktrees: linked,
		})
	}
	sort.Slice(repositories, func(i, j int) bool {
		return repositories[i].Name < repositories[j].Name
	})
	return repositories, nil
}

func findGitRoots(root string) ([]string, error) {
	var roots []string
	var visit func(string) error
	visit = func(directory string) error {
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			roots = append(roots, directory)
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
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
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "detached":
			current.Detached = true
		case line == "locked" || strings.HasPrefix(line, "locked "):
			current.Locked = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			current.Prunable = true
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

func readClaudeSessions() map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	home, err := os.UserHomeDir()
	if err != nil {
		return result
	}
	base := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasPrefix(entry.Name(), "local_") || filepath.Ext(path) != ".json" {
			return nil
		}
		var session struct {
			SessionID  string
			CWD        string
			IsArchived *bool
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &session) != nil || session.IsArchived == nil || *session.IsArchived || session.CWD == "" {
			return nil
		}
		cwd, err := canonicalPath(session.CWD)
		if err != nil {
			return nil
		}
		if result[cwd] == nil {
			result[cwd] = make(map[string]bool)
		}
		result[cwd][session.SessionID] = true
		return nil
	})
	return result
}

func readCodexSessions(ctx context.Context) map[string]int {
	result := make(map[string]int)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return result
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return result
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	database := filepath.Join(codexHome, "sqlite", "state_5.sqlite")
	output, err := exec.CommandContext(ctx, "sqlite3", "-separator", "\t", database,
		"SELECT cwd, COUNT(*) FROM threads WHERE archived = 0 GROUP BY cwd;").Output()
	if err != nil {
		return result
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		separator := strings.LastIndexByte(line, '\t')
		if separator == -1 {
			continue
		}
		count, err := strconv.Atoi(line[separator+1:])
		if err != nil {
			continue
		}
		cwd, err := canonicalPath(line[:separator])
		if err == nil {
			result[cwd] = count
		}
	}
	return result
}

func removeWorktree(ctx context.Context, repo repository, item worktree, deleteBranch bool) deletionResult {
	result := deletionResult{Path: item.Path, Branch: item.Branch, Missing: item.Missing}
	if _, err := git(ctx, repo.MainPath, "worktree", "remove", "--force", "--force", item.Path); err != nil {
		result.Err = err
		return result
	}
	result.Removed = true
	if deleteBranch && item.Branch != "" {
		if _, err := git(ctx, repo.MainPath, "branch", "-D", item.Branch); err != nil {
			result.Err = fmt.Errorf("worktree removed; branch not deleted: %w", err)
			return result
		}
		result.BranchDeleted = true
	}
	return result
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
