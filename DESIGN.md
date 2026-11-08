# Space Review

Space Review is a Windows x64 executable with a native desktop UI, SQLite, and a stdio MCP server. The index defaults to %LOCALAPPDATA%\SpaceReview\index.sqlite3. To keep the database with a portable copy, launch either mode with --index followed by the local database path.

## What the research informed

- WizTree uses the NTFS master file table for fast whole-volume discovery. Space Review uses batched Windows directory records for allocation size and file identity under normal user permissions. It skips junctions, counts hard links once per scan, and measures locally allocated bytes for cloud files. Directory records report no allocation for WOF-compressed files (CompactOS, `compact /exe`), so the scan asks Windows for their compressed size. [WizTree](https://diskanalyzer.com/) · [Windows directory records](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_id_extd_dir_info)
- TreeSize makes saved snapshots and comparisons central to finding growth. WinDirStat emphasizes large files and visual drill-down. Space Review ranks signed changes by magnitude, shows before and after values, and separates actual free-space movement from the net change in comparable folders. [TreeSize comparisons](https://manuals.jam-software.de/treesize/EN/disk_usage_comparison.html) · [WinDirStat](https://github.com/windirstat/windirstat)
- An NTFS journal checkpoint is a useful freshness signal. Journal records identify changes and reasons. A fresh capture still reads allocation metadata to get byte counts. Journal-based subtree refresh would need to invalidate its cache after a gap or journal reset and account for moves and hard links. [Change journal records](https://learn.microsoft.com/en-us/windows/win32/fileio/change-journal-records) · [Journal continuity](https://learn.microsoft.com/en-us/windows/win32/api/winioctl/ns-winioctl-usn_journal_data_v2)
- Go fits the portable distribution. On this PC, a 676,834-file scan took 6.1 seconds in the Python prototype. Native captures of the same scope took 4.5 and 6.5 seconds including the SQLite commit and MCP round trip. The executable also opens saved results without a directory walk.

## History

Each completed capture saves its scope, timestamp, coverage, drive readings, and size summaries in one SQLite transaction. Unchanged readings reuse their existing rows. Comparisons query only the versions that open or close between the two endpoints. Saving a capture keeps 120 days for that scope and its nearest earlier baseline. A capture is refused only when another capture finished after it. A saved time that is still ahead of the clock means the clock moved back, so the capture saves, and retention removes only captures older than every capture inside the window.

Folder history is tied to the volume that holds the folder. Quick review history spans every fixed drive and continues when drives are added or removed. Its comparisons count a path only when its drive holds the same volume in both captures.

The index uses write-ahead logging on fixed drives when available, with SQLite's full synchronous setting. Desktop and MCP readers can view the last committed history while a capture saves. Close both modes before copying or moving the index so SQLite can checkpoint its pending writes. [SQLite write-ahead logging](https://sqlite.org/wal.html)

Startup displays the latest saved results immediately and refreshes older file results when drive or journal readings indicate activity. Quick review captures daily while selected and the app is open. Docker captures daily while its tab is open. Explicit scans always measure the selected scope.

Comparisons use the latest capture and the nearest saved baseline at or before the chosen cutoff. They match Windows file identities first and pair replacements at the same path. The UI shows the actual dates, marks shorter history, and omits changes affected by incomplete coverage. Large files and the immediate subfolders of ranked folders are tracked from 64 MiB. Threshold crossings are displayed as conservative bounds. Subfolder comparisons start once both captures contain subfolder readings. Docker resource deltas remain separate from Windows virtual disk allocation. [Docker disk usage](https://docs.docker.com/reference/cli/docker/system/df/)

## MCP

Launch the same executable with --mcp, or copy the local stdio configuration from the MCP tab. The server exposes saved scopes, largest items, changes, and on-demand captures. A capture writes a snapshot to the index. Storage inspection uses read-only filesystem metadata and local Docker API requests. [MCP stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)

## Desktop interaction

Changes opens first so the largest gains and losses are easy to find. Each view has one capture action. The shared status strip keeps progress and cancellation available across tabs while saved results stay visible. Switching a location loads that location’s saved scan, and returning from folder inspection restores the previous search and selection. [Apple’s design principles](https://developer.apple.com/design/human-interface-guidelines/design-principles) · [Feedback](https://developer.apple.com/design/human-interface-guidelines/feedback)

Native Windows controls provide focus and selection states. F5 captures the current location or refreshes Docker, and Ctrl+F focuses the search field. Signed amounts and text communicate changes alongside the bars. [Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility)

The window opens while the index loads. Index reads, drive probes, and Explorer launches run on workers. New selections cancel older reads, and sorting preserves the selected item's identity. Search reuses the current view's prepared rows and sorts a list of visible row indexes.

## Diagnostics

Open Help → Open diagnostics folder. The desktop and MCP share newline-delimited JSON logs in the diagnostics folder beside the selected index. The default is %LOCALAPPDATA%\SpaceReview\diagnostics. The active log and one previous log each have a 2 MiB limit. A Windows byte-range lock coordinates appends and rotation across processes. [Windows file locking](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)

Group records by `session` and `op` to follow a capture or comparison. `scan.files` separates discovery, traversal, and finalization and counts files, metadata batches, fallback reads, and coverage gaps. `index.save` records writer wait, version reuse, and commit time. `index.compare` separates SQLite reads from change matching. `docker.api` records response status, bytes, and header and decode times. `mcp.request` times requests. `gui.compare` and `gui.capture` include the UI queue and display update times. Durations are in milliseconds.

Logging is local and enabled at info level. Launch either mode with `--log-level debug` for fast UI render timings, or `--log-level off` to disable logging. Slow renders are logged at info settings. Error codes and sanitized Go panic stacks help locate bugs. Logs omit file paths, search text, MCP arguments, Docker names, and raw error messages. The bounded queue keeps filesystem writes off the UI thread and reports dropped records when it can resume writing.

## Testing

Run `go test -count=1 ./...` and `go vet ./...` in this folder. The tests cover Windows allocation and hard links, SQLite history, concurrent access and rollback, Docker usage parsing, and diagnostics rotation, contention, error reporting, and privacy.

Set `SPACE_REVIEW_EXE` to the absolute path to a built Windows executable to run the portable MCP tests. They exercise real file changes across restart, concurrent captures and shared logs, and continued operation when logging is disabled or unavailable. Set `SPACE_REVIEW_TEST_LIVE_DOCKER=1` to also verify usage and timings against the local Docker engine.
