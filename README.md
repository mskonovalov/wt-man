# wt-man

wt-man is an interactive manager for Git worktrees spread across many repositories.

It scans ~/work by default, groups linked worktrees by repository, and orders each group by creation time. The terminal UI supports filtering, multi-selection, unarchived Claude/Codex session warnings, bulk removal, and optional local branch deletion.

## Install

~~~sh
go install github.com/mskonovalov/wt-man@latest
~~~

Or build the current checkout:

~~~sh
go build .
~~~

## Usage

~~~sh
wt-man
wt-man --root /path/to/workspace
~~~

Keys:

- Up/Down or j/k: move
- Space: select
- a: select or clear all visible worktrees
- /: filter by repository, branch, or path
- u: cycle between all, with unarchived sessions, and without unarchived sessions
- Enter: review selected worktrees
- x: toggle local branch deletion on the review screen
- D: confirm deletion from the review screen
- q: quit

Deletion uses git worktree remove --force --force. The review screen always appears before anything is removed.
For an existing worktree, deletion removes both its directory and Git's worktree record. `missing (prunable)` means the directory no longer exists but Git still has a stale record; deleting it removes only that record.

Claude session status is read from Claude Desktop's local session JSON files. Codex status is read from its local SQLite state database when sqlite3 is available. These are internal formats, so session detection is best-effort.
