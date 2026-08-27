package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
)

type cursorSessionProvider struct{}

func (cursorSessionProvider) Name() string {
	return "Cursor"
}

func (cursorSessionProvider) Sessions(ctx context.Context) ([]agentSession, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	database := filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	if _, err := os.Stat(database); err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(ctx, "sqlite3", "-json", database,
		"SELECT CAST(value AS TEXT) AS value FROM ItemTable WHERE key = 'composer.composerHeaders';").Output()
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, os.ErrNotExist
	}
	var stored struct {
		AllComposers []struct {
			ComposerID          string `json:"composerId"`
			Name                string `json:"name"`
			Subtitle            string `json:"subtitle"`
			CreatedAt           int64  `json:"createdAt"`
			LastUpdatedAt       int64  `json:"lastUpdatedAt"`
			IsArchived          *bool  `json:"isArchived"`
			WorkspaceIdentifier struct {
				URI *struct {
					FSPath string `json:"fsPath"`
				} `json:"uri"`
			} `json:"workspaceIdentifier"`
		} `json:"allComposers"`
	}
	if err := json.Unmarshal([]byte(rows[0].Value), &stored); err != nil {
		return nil, err
	}
	var sessions []agentSession
	for _, composer := range stored.AllComposers {
		if composer.ComposerID == "" || composer.WorkspaceIdentifier.URI == nil || composer.WorkspaceIdentifier.URI.FSPath == "" {
			continue
		}
		cwd, err := canonicalPath(composer.WorkspaceIdentifier.URI.FSPath)
		if err != nil {
			continue
		}
		archiveStatus := sessionArchiveUnknown
		if composer.IsArchived != nil {
			archiveStatus = sessionArchiveUnarchived
			if *composer.IsArchived {
				archiveStatus = sessionArchiveArchived
			}
		}
		title := composer.Name
		if title == "" {
			title = composer.Subtitle
		}
		updatedAt := composer.LastUpdatedAt
		if updatedAt == 0 {
			updatedAt = composer.CreatedAt
		}
		sessions = append(sessions, agentSession{
			ID: composer.ComposerID, WorkingDirectory: cwd, Title: title,
			UpdatedAt: sessionTime(updatedAt), ArchiveStatus: archiveStatus,
		})
	}
	return sessions, nil
}
