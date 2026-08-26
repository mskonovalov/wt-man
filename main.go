package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

func main() {
	root := flag.String("root", ".", "directory containing Git repositories")
	flag.Parse()
	if flag.NArg() > 0 {
		*root = flag.Arg(0)
	}

	m := newDiscoveringModel(*root)
	program := tea.NewProgram(m)
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "wt-man:", err)
		os.Exit(1)
	}
}
