package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

type sessionArchiveStatus int

const (
	sessionArchiveUnknown sessionArchiveStatus = iota
	sessionArchiveUnarchived
	sessionArchiveArchived
)

type agentSession struct {
	ID               string
	WorkingDirectory string
	Title            string
	Model            string
	URL              string
	UpdatedAt        time.Time
	ArchiveStatus    sessionArchiveStatus
}

type sessionProvider interface {
	Name() string
	Sessions(context.Context) ([]agentSession, error)
	MoveHint(agentSession, string) string
}

type sessionProviderResult struct {
	Provider sessionProvider
	Name     string
	Sessions []agentSession
	Err      error
}

type worktreeSessions struct {
	Providers []worktreeSessionProvider
}

type worktreeSessionProvider struct {
	Provider sessionProvider
	Name     string
	Known    bool
	Sessions []agentSession
}

var sessionProviders = []sessionProvider{
	claudeSessionProvider{},
	codexSessionProvider{},
	cursorSessionProvider{},
}

func readSessionProviders(ctx context.Context, providers []sessionProvider) []sessionProviderResult {
	results := make([]sessionProviderResult, len(providers))
	var wait sync.WaitGroup
	wait.Add(len(providers))
	for index, provider := range providers {
		go func() {
			defer wait.Done()
			sessions, err := provider.Sessions(ctx)
			results[index] = sessionProviderResult{Provider: provider, Name: provider.Name(), Sessions: sessions, Err: err}
		}()
	}
	wait.Wait()
	return results
}

func unknownSessionProviders(providers []sessionProvider) []worktreeSessionProvider {
	result := make([]worktreeSessionProvider, len(providers))
	for index, provider := range providers {
		result[index].Provider = provider
		result[index].Name = provider.Name()
	}
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sessionTime(milliseconds int64) time.Time {
	if milliseconds == 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds)
}

func (sessions worktreeSessions) unarchivedCount() int {
	total := 0
	for _, provider := range sessions.Providers {
		for _, session := range provider.Sessions {
			if session.ArchiveStatus == sessionArchiveUnarchived {
				total++
			}
		}
	}
	return total
}

func (provider worktreeSessionProvider) unarchivedSessions() []agentSession {
	var sessions []agentSession
	for _, session := range provider.Sessions {
		if session.ArchiveStatus == sessionArchiveUnarchived {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func (provider worktreeSessionProvider) visibleSessions() []agentSession {
	var sessions []agentSession
	for _, session := range provider.Sessions {
		if session.ArchiveStatus != sessionArchiveArchived {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func (provider worktreeSessionProvider) archiveStatusKnown() bool {
	if !provider.Known {
		return false
	}
	for _, session := range provider.Sessions {
		if session.ArchiveStatus == sessionArchiveUnknown {
			return false
		}
	}
	return true
}

func (sessions worktreeSessions) archiveStatusKnown() bool {
	if len(sessions.Providers) == 0 {
		return false
	}
	for _, provider := range sessions.Providers {
		if !provider.archiveStatusKnown() {
			return false
		}
	}
	return true
}
