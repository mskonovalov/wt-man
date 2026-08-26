# wt-man

wt-man is an interactive manager for Git worktrees spread across many repositories.

It scans ~/work by default and groups linked worktrees by repository. On macOS, each group is ordered by filesystem creation time; on other platforms, it retains Git's worktree-list order because a reliable creation time may be unavailable. The terminal UI supports filtering, multi-selection, unarchived Claude/Codex session warnings, bulk removal, and optional local branch deletion.

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
- Page Up/Page Down: move by one visible page
- Space: select
- a: select or clear all visible worktrees
- /: filter by repository, branch, or path
- u: cycle between all, with unarchived sessions, and without unarchived sessions
- r: refresh the selected worktree's modified date
- R: refresh all modified dates
- Enter: review selected worktrees
- x: toggle local branch deletion on the review screen
- D: confirm deletion from the review screen
- q: quit

Deletion uses `git worktree remove --force`. Locked worktrees are identified in the list and review screen and are refused until you explicitly unlock them with Git. The review screen always appears before anything is removed.
For an existing worktree, deletion removes both its directory and Git's worktree record. `missing (Git record only)` means the directory no longer exists but Git still has a stale record; `; prunable` is added when Git reports that state. Deleting either missing state removes only the record.
Optional branch cleanup uses Git's safe `git branch -d` check, so an unmerged local branch is kept even after its worktree has been removed.

On macOS, the created column uses the worktree directory's filesystem birth time. It displays `unknown` on platforms where a reliable creation timestamp is unavailable. The modified column is the newest filesystem modification time anywhere under the worktree; it is scanned in the background and does not follow symbolic links. Results are cached for 24 hours in the operating system's user cache directory.
Date scans process one worktree at a time and show progress above the list. Deletion pauses further scans, removes one selected worktree at a time, and shows the current path and batch progress.
Repository and branch columns expand to show their full values. The path column uses the remaining terminal width and is hidden when space is limited; on narrower terminals, the full branch moves onto a second line. The selected worktree's full details remain below the list.

The merged column checks whether the local branch tip is already contained in the repository's locally available default branch ref. It prefers `origin/HEAD`, then falls back to `origin/main`, `origin/master`, `main`, or `master`; run `git fetch` first when you need current remote state. A merged branch can still have uncommitted worktree changes, so this is evidence about committed history rather than a complete safety guarantee.

Claude session status is read from Claude Desktop's local session JSON files. Codex status is read from its local SQLite state database when sqlite3 is available. These are internal formats, so session detection is best-effort. `C?` or `X?` means that provider could not be checked; the "without unarchived" filter only shows a worktree when both providers were checked successfully.
