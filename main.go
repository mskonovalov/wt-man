package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

func main() {
	root := flag.String("root", defaultRoot(), "directory containing Git repositories")
	flag.Parse()
	if flag.NArg() > 0 {
		*root = flag.Arg(0)
	}

	repositories, githubAuthenticated, err := discover(context.Background(), *root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-man:", err)
		os.Exit(1)
	}
	if len(repositories) == 0 {
		fmt.Println("No linked Git worktrees found.")
		return
	}

	m := newModel(repositories)
	m.githubAuthAvailable = githubAuthenticated
	program := tea.NewProgram(m)
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "wt-man:", err)
		os.Exit(1)
	}
}

func defaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "work")
}
