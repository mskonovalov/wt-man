package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type claudeSessionProvider struct{}

func (claudeSessionProvider) Name() string {
	return "Claude"
}

func (claudeSessionProvider) MoveHint(session agentSession, destination string) string {
	return "Claude: cd " + shellQuote(destination) + " && claude --resume " + shellQuote(session.ID)
}

func (claudeSessionProvider) Sessions(context.Context) ([]agentSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	if _, err := os.Stat(base); err != nil {
		return nil, err
	}

	sessionsByDirectory := make(map[string]map[string]agentSession)
	var sourceErr error
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			sourceErr = errors.Join(sourceErr, walkErr)
			return nil
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "local_") || filepath.Ext(path) != ".json" {
			return nil
		}
		var stored struct {
			SessionID      string
			CWD            string
			IsArchived     *bool
			Title          string
			Model          string
			CreatedAt      int64
			LastActivityAt int64
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			sourceErr = errors.Join(sourceErr, readErr)
			return nil
		}
		if unmarshalErr := json.Unmarshal(data, &stored); unmarshalErr != nil {
			sourceErr = errors.Join(sourceErr, unmarshalErr)
			return nil
		}
		if stored.IsArchived == nil || stored.SessionID == "" || stored.CWD == "" {
			sourceErr = errors.Join(sourceErr, errors.New("incomplete Claude session"))
			return nil
		}
		cwd, canonicalErr := canonicalPath(stored.CWD)
		if canonicalErr != nil {
			sourceErr = errors.Join(sourceErr, canonicalErr)
			return nil
		}
		archiveStatus := sessionArchiveUnarchived
		if *stored.IsArchived {
			archiveStatus = sessionArchiveArchived
		}
		updatedAt := stored.LastActivityAt
		if updatedAt == 0 {
			updatedAt = stored.CreatedAt
		}
		if sessionsByDirectory[cwd] == nil {
			sessionsByDirectory[cwd] = make(map[string]agentSession)
		}
		sessionsByDirectory[cwd][stored.SessionID] = agentSession{
			ID: stored.SessionID, WorkingDirectory: cwd, Title: stored.Title, Model: stored.Model,
			URL: "claude://resume?session=" + url.QueryEscape(stored.SessionID), UpdatedAt: sessionTime(updatedAt), ArchiveStatus: archiveStatus,
		}
		return nil
	})
	if err != nil {
		sourceErr = errors.Join(sourceErr, err)
	}
	var sessions []agentSession
	for _, byID := range sessionsByDirectory {
		for _, session := range byID {
			sessions = append(sessions, session)
		}
	}
	return sessions, sourceErr
}
