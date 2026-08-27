package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
)

type codexSessionProvider struct{}

func (codexSessionProvider) Name() string {
	return "Codex"
}

func (codexSessionProvider) Sessions(ctx context.Context) ([]agentSession, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	database := filepath.Join(codexHome, "sqlite", "state_5.sqlite")
	if _, err := os.Stat(database); err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(ctx, "sqlite3", "-json", database,
		"SELECT id, cwd, title, COALESCE(model, '') AS model, COALESCE(updated_at_ms, updated_at * 1000) AS updated_at_ms, archived FROM threads;").Output()
	if err != nil {
		return nil, err
	}
	var stored []struct {
		ID          string `json:"id"`
		CWD         string `json:"cwd"`
		Title       string `json:"title"`
		Model       string `json:"model"`
		UpdatedAtMS int64  `json:"updated_at_ms"`
		Archived    int    `json:"archived"`
	}
	if len(output) > 0 {
		if err := json.Unmarshal(output, &stored); err != nil {
			return nil, err
		}
	}
	sessions := make([]agentSession, 0, len(stored))
	var sourceErr error
	for _, storedSession := range stored {
		cwd, err := canonicalPath(storedSession.CWD)
		if err != nil {
			sourceErr = errors.Join(sourceErr, fmt.Errorf("resolve Codex session directory: %w", err))
			continue
		}
		archiveStatus := sessionArchiveUnarchived
		if storedSession.Archived != 0 {
			archiveStatus = sessionArchiveArchived
		}
		sessions = append(sessions, agentSession{
			ID: storedSession.ID, WorkingDirectory: cwd, Title: storedSession.Title, Model: storedSession.Model,
			URL: "codex://threads/" + url.PathEscape(storedSession.ID), UpdatedAt: sessionTime(storedSession.UpdatedAtMS), ArchiveStatus: archiveStatus,
		})
	}
	return sessions, sourceErr
}
