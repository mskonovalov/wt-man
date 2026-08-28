package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type worktreeMoveResult struct {
	Source      string
	Destination string
	Moved       bool
	Err         error
}

func worktreeMoveDestination(repo repository, item worktree, parent string) (string, error) {
	if err := worktreeMoveUnavailable(repo, item); err != nil {
		return "", err
	}

	parent, err := canonicalPath(parent)
	if err != nil {
		return "", fmt.Errorf("resolve destination folder: %w", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect destination folder: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("destination parent is not a directory: %s", parent)
	}

	name := filepath.Base(filepath.Clean(item.Path))
	if name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("cannot determine the worktree folder name")
	}
	destination := filepath.Join(parent, name)
	source, _ := canonicalPath(item.Path)
	if source == destination {
		return "", fmt.Errorf("worktree is already in this folder")
	}
	if relative, relativeErr := filepath.Rel(source, destination); relativeErr == nil {
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return "", fmt.Errorf("cannot move a worktree inside itself")
		}
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("destination already exists: %s", destination)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect destination: %w", err)
	}
	return destination, nil
}

func createMoveDirectory(parent, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("folder name must be a single directory name")
	}
	parent, err := canonicalPath(parent)
	if err != nil {
		return "", fmt.Errorf("resolve parent folder: %w", err)
	}
	destination := filepath.Join(parent, name)
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("folder already exists: %s", destination)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect new folder: %w", err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		return "", fmt.Errorf("create folder: %w", err)
	}
	return canonicalPath(destination)
}

func worktreeMoveUnavailable(repo repository, item worktree) error {
	if item.Missing {
		return fmt.Errorf("cannot move a missing worktree")
	}
	if item.Broken {
		return fmt.Errorf("cannot move a broken worktree")
	}
	if item.Locked {
		return fmt.Errorf("cannot move a locked worktree; unlock it first")
	}
	source, err := canonicalPath(item.Path)
	if err != nil {
		return fmt.Errorf("resolve worktree path: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect worktree path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("worktree path is not a directory: %s", source)
	}
	mainPath, err := canonicalPath(repo.MainPath)
	if err == nil && source == mainPath {
		return fmt.Errorf("Git cannot move the main worktree")
	}
	return nil
}

func moveWorktree(ctx context.Context, repo repository, item worktree, parent string) worktreeMoveResult {
	result := worktreeMoveResult{Source: item.Path}
	destination, err := worktreeMoveDestination(repo, item, parent)
	result.Destination = destination
	if err != nil {
		result.Err = err
		return result
	}
	if _, err := git(ctx, repo.MainPath, "worktree", "move", item.Path, destination); err != nil {
		result.Err = err
		return result
	}
	result.Moved = true
	if err := verifyPathRemoved(item.Path); err != nil {
		result.Err = fmt.Errorf("verify old worktree path removed: %w", err)
		return result
	}
	return result
}
