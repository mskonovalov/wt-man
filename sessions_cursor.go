package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	database := cursorDatabasePath(home)
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
	if len(output) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) > 1 {
		return nil, fmt.Errorf("read Cursor composer headers: got %d rows", len(rows))
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
	var sourceErr error
	for _, composer := range stored.AllComposers {
		if composer.ComposerID == "" {
			sourceErr = errors.Join(sourceErr, errors.New("Cursor session has no composer ID"))
			continue
		}
		if composer.WorkspaceIdentifier.URI == nil || composer.WorkspaceIdentifier.URI.FSPath == "" {
			sourceErr = errors.Join(sourceErr, fmt.Errorf("Cursor session %s has no workspace path", composer.ComposerID))
			continue
		}
		cwd, err := canonicalPath(composer.WorkspaceIdentifier.URI.FSPath)
		if err != nil {
			sourceErr = errors.Join(sourceErr, fmt.Errorf("resolve Cursor session directory: %w", err))
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
	return sessions, sourceErr
}

func cursorDatabasePath(home string) string {
	if runtime.GOOS == "linux" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, "Cursor", "User", "globalStorage", "state.vscdb")
	}
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
}
