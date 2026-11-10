# Space Review

Space Review is a Windows x64 executable with a GPU-drawn desktop UI built with Gio, SQLite, a stdio MCP server, and a local gRPC API. The index defaults to `%LOCALAPPDATA%\SpaceReview\index.sqlite3`. Use `--index` to select a local database for a portable copy.

## Build and run

With the Go version in `go.mod` on PATH, build the Windows x64 executable from the repository root in PowerShell.

```powershell
go build -trimpath -ldflags '-H=windowsgui' -o '.\bin\Space Review.exe' ./cmd/spacereview
& '.\bin\Space Review.exe'
```

The desktop and MCP modes open the index directly by default. To share a single backend process, start the API server, then connect the desktop or the stdio MCP server to it.

```powershell
& '.\bin\Space Review.exe' --serve
& '.\bin\Space Review.exe' --connect 127.0.0.1:47820
& '.\bin\Space Review.exe' --mcp --connect 127.0.0.1:47820
```

Use `--listen` on the server to select another loopback address. For a custom index, pass `--index` to the server and `--token-file` with the path to its `api-token` to each client. The server creates that file beside its index on a filesystem that supports Windows ACLs. It grants access to the Windows account running the server and rotates that account's inherited tokens from earlier builds at startup. Clients must run as the same Windows account. When using an explicit `--token-file` on the server, give the clients the same path. The versioned API is defined in [space_review.proto](proto/spacereview/v1/space_review.proto).

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

Changes can compare the latest capture with a preset baseline or any two saved scans of the same location. Presets use the nearest eligible scan at or before the cutoff. The dates appear beside the comparison controls. Comparisons match file identities first and pair replacements at the same path. They require matching scan exclusions and omit readings affected by incomplete coverage. Large files and the immediate subfolders of ranked folders are tracked in history from 64 MiB. The change table marks threshold crossings as lower bounds. Docker resource deltas are shown separately from Windows virtual disk allocation. [Docker disk usage](https://docs.docker.com/reference/cli/docker/system/df/)

## File exploration

Scan from **Files** to save the full file and folder inventory for that location. The index keeps one paged inventory per scope, alongside compact history. Files ranks individual allocations at any depth. Folders shows the current level, and **Inspect folder** opens its children from the saved inventory. Back restores the filters, sort, page, and selection. **Scan coverage** explains allocated space, unreadable locations, estimates, and exclusions. Whole-drive results also show used space the scan could not attribute.

Files checks the paths in the current results when you open a view or return to the window. It hides entries Windows confirms are missing. Scan again to update sizes and find new items.

**Filters** narrows the current view by cleanup guidance, path, size, modification date, and, in Largest files, extension. **Apply filters** updates the table, and **Cancel** discards draft changes. The button shows how many filters are active. **Scan exclusions** changes what the next scan measures. **Treemap** uses the same allocations as the current page. Select a tile to highlight its row. **Inspect folder** opens the selected folder.

In **Files**, select an item and choose **Details** → **Delete…**. Confirm the full path to move an eligible file or the entire folder to the Windows Recycle Bin. The app rescans the location after the move. Items in the Recycle Bin still use disk space. [Windows shell file operations](https://learn.microsoft.com/en-us/windows/win32/api/shobjidl_core/nf-shobjidl_core-ifileoperation-setoperationflags)

**Export** saves the current filtered view as CSV or JSON, including the scan times and coverage. A detailed file export streams matching saved rows in the current sort order and skips paths hidden on the current page. CSV uses integer byte counts and escapes text that a spreadsheet could interpret as a formula.

**Find duplicates** checks same-length files against their saved metadata and reads their full contents locally. It skips online-only, reparse, busy, and changed files. Each group counts distinct physical copies. Hard-link paths appear under their copy, and an allocation is counted as reclaimable only when every link to that copy is in the result. Mark the copy to keep and use Explorer to review the paths.

## Scheduled captures

**Schedule scans** registers a daily Windows Task Scheduler job for the current location and index. The job uses the selected executable and starts while you are signed in and Windows is awake, idle and on AC power, even with the Space Review window closed. Windows stops it when activity resumes or the PC switches to battery, and retries an idle interruption when the PC is idle again. The capture runs at background CPU and disk I/O priority. Save an existing schedule again to apply the idle and power conditions. [Windows idle conditions](https://learn.microsoft.com/en-us/windows/win32/taskschd/task-idle-conditions) · [Windows background priority](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-setpriorityclass)

The dialog shows the next scheduled time and last recorded result. Scheduled jobs skip the index if another scan is running. They use the location’s latest exclusions and detail settings.

An optional free-space threshold shows a local Windows notification when a monitored volume first reaches or drops below it. A later reading above the threshold rearms the alert. The schedule dialog keeps the last low-space reading and run result. [Windows Task Scheduler](https://learn.microsoft.com/en-us/windows/win32/taskschd/schtasks) · [Windows notification area](https://learn.microsoft.com/en-us/windows/win32/shell/notification-area)

## Labels and cleanup

Every measured file counts toward one category, and every category has a removal level:

- **Likely reclaimable** · validated cache directories, known tool-managed caches, Windows temporary files and the Recycle Bin. MCP calls this level **Safe to clear**.
- **Check first** · possible caches and logs, build output and installed dependencies, downloads, backups, checkouts and worktrees, scratch folders, virtual machine and emulator disks, Docker storage and the previous Windows installation.
- **Uninstall** · programs and games. Remove them in Settings > Apps or in their launcher, because deleting the folder leaves registrations behind.
- **Keep** · Windows, app data, projects and personal files.

Labels come from known Windows and tool locations and from markers in each folder. A `.git`, `.hg` or `.svn` folder or a project manifest marks a project. A local `CACHEDIR.TAG` with the specified signature can mark a cache, and launcher manifests or a Unity runtime can mark a game. Names such as `cache`, `logs` and `dist` go to **Check first**. Older captures remain readable and need a fresh scan to qualify for **Safe to clear**. [Cache directory tagging](https://bford.info/cachedir/)

Captures record a total per category, the top folder of each cleanup category as a review candidate, and the top folder of each program, game and project. Review candidates and labeled folders count the bytes assigned to their own category. A tagged cache inside a worktree appears separately under **Likely reclaimable**. The Files summary opens the matching items, largest first. The inspector shows the evidence for the label, measured space, and the next step, with links to Windows Storage, Installed apps, the Recycle Bin, or Docker where applicable. [Windows Settings links](https://learn.microsoft.com/en-us/windows/apps/develop/launch/launch-settings)

Captures store the version of the labeling rules. A review candidate or labeled folder is comparable when both captures measured it under the same category and labeling rules. A row that disappeared after a rename or a change of category is omitted. The drive readings show the actual movement in free space. The walk reads a folder's listing before counting its files and checks the signature of local cache tags.

## MCP

The MCP view connects assistants to the same saved history. Storage inspection uses read-only filesystem metadata and local Docker API requests. Tool results can include file paths, sizes, and scan times. A capture saves a snapshot to the index. [MCP stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)

- **Claude Desktop** · Save the `.mcpb` extension and open it in Claude Desktop to install. The extension bundles the current Space Review executable. Export it again after upgrading Space Review or switching history locations. [Claude Desktop extensions](https://support.claude.com/en/articles/10949351-getting-started-with-local-mcp-servers-on-claude-desktop)
- **Local Codex or Claude Code** · Copy the matching setup command and run it in PowerShell with that CLI installed. Start a new local chat and check `/mcp`. In the ChatGPT desktop app, you can also add the command and arguments as a STDIO server under Settings → MCP servers. [Codex MCP setup](https://learn.chatgpt.com/docs/extend/mcp#connect-codex-to-an-mcp-server) · [Claude Code MCP setup](https://code.claude.com/docs/en/mcp#option-3-add-a-local-stdio-server)
- **ChatGPT web** · Use the command and arguments from Copy stdio JSON as the Secure MCP Tunnel's stdio target. In ChatGPT Plugins, choose Add custom MCP server, then Tunnel. The PC and tunnel client must be running, and the account must have access to the tunnel and custom MCP servers. [OpenAI Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels)

Ask “Use Space Review to show my ten largest files and the folders using the most space.” The answer should include the saved scan time. For a coding session, scan the same project folder before and after. Ask “What grew most between those scans?” `storage_changes` with `days=0` compares the last two captures of the same scope. Check the two capture times against the session. After scanning a drive, ask “What can I safely clean up on C:?” `storage_breakdown` totals the capture by category and removal level and lists the largest review candidates and the largest programs, games and projects.

## Ask assistant

Files, Changes, and Docker can prepare an editable question with up to ten of the largest measured rows from the current filtered view. The draft includes display names, sizes, scan times, and coverage limits. Choose ChatGPT desktop or Claude to prefill a new chat using the installed app's sign-in. ChatGPT web and desktop launch fallbacks copy the prompt for pasting. Sending the draft happens in the assistant. This flow works with saved scans independently of the MCP connection. [ChatGPT desktop links](https://learn.chatgpt.com/docs/app/commands#deep-links) · [Claude Desktop links](https://support.claude.com/en/articles/14729294-open-claude-desktop-with-a-link)

## Desktop interaction

Changes opens first. After the first file scan, **View what uses space** opens the saved results in Files. The sidebar holds navigation and drive capacity. The workspace gives the ranked results most of the width and separates drive usage from changes in comparable folders. **Details** shows the selected item and its actions. Select a bar in **Largest changes** to jump to its row and show its details. [Little Snitch Network Monitor](https://help.obdev.at/littlesnitch6/lsm-overview) · [Cohesity Data Cloud](https://ux-design-awards.com/winners/2026-2-cohesity-data-cloud) · [beQ Dashboard](https://ux-design-awards.com/winners/2026-1-beq-dashboard-smart-investment-reward-control) · [Apple split views](https://developer.apple.com/design/human-interface-guidelines/split-views)

Each review view has one capture action at the right edge of the toolbar. **Cancel scan** takes the same spot during a scan, with progress and saved scan dates in the status strip. **More** groups occasional actions such as export, scheduling, and scan settings. Location selectors keep long saved paths from shifting nearby controls. Open the selector to read the path. In Files, **Back** sits beside the results heading and restores the search, filters, sort order, and selection after folder inspection or a cleanup summary shortcut. [Apple toolbars](https://developer.apple.com/design/human-interface-guidelines/toolbars) · [Feedback](https://developer.apple.com/design/human-interface-guidelines/feedback)

The window is drawn with Gio on the GPU, so the workspace matches the concept instead of the classic Windows controls. The title bar, palette, and focus colors follow the Windows light, dark, and high contrast settings. Folder and save choices use the Windows common item dialogs. [Gio](https://gioui.org/)

Below 960 dp the sidebar becomes an icon rail. When the workspace is at least 680 dp wide, **Details** and **Treemap** share a pane beside the table. In narrower workspaces they open on demand in a dialog. The search and view controls stay above the table. Short, narrow windows show the leading summary measurement and open the full **Summary** in a dialog. Narrow tables hide Before, Modified, or Activity before shortening values.

Tab moves keyboard focus with a visible ring. In the table, the arrow, Page Up, Page Down, Home, and End keys move the selection, Enter opens the selected item, and Ctrl+C copies its path. F5 captures the current location or refreshes Docker, Ctrl+F focuses search, and Ctrl+I toggles **Details**. Escape closes the menu or dialog in focus. The controls show their shortcuts on hover and keyboard focus. The Windows UI Automation provider exposes visible text, controls and rows to screen readers. Move through results with the table keys to bring more rows into view. [Windows UI Automation providers](https://learn.microsoft.com/en-us/windows/win32/winauto/uiauto-providerportal)

The window opens while the index loads. Index reads, drive probes, and Explorer launches run on workers. New selections cancel older reads, and sorting preserves the selected item's identity. Search reuses the current view's prepared rows and sorts a list of visible row indexes.

## Diagnostics

Select Diagnostics at the bottom of the sidebar. The desktop and MCP share newline-delimited JSON logs in the diagnostics folder beside the selected index. The default is %LOCALAPPDATA%\SpaceReview\diagnostics. The active log and one previous log each have a 2 MiB limit. A Windows byte-range lock coordinates appends and rotation across processes. [Windows file locking](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)

Group records by `session` and `op` to follow a capture or comparison. `scan.files` separates discovery, traversal, and finalization and counts files, metadata batches, fallback reads, and coverage gaps. `index.save` records writer wait, version reuse, and commit time. `index.compare` separates SQLite reads from change matching. `docker.api` records response status, bytes, and header and decode times. `mcp.request` times requests. `gui.compare` and `gui.capture` include the UI queue and display update times. Durations are in milliseconds.

Logging is local and enabled at info level. Launch either mode with `--log-level debug` for fast UI render timings, or `--log-level off` to disable logging. Slow renders are logged at info settings. Error codes and sanitized Go panic stacks help locate bugs. Logs omit file paths, search text, MCP arguments, Docker names, and raw error messages. The bounded queue keeps filesystem writes off the UI thread and reports dropped records when it can resume writing.

## Testing

Run `go test -count=1 ./...` and `go vet ./...` in this folder. Windows CI builds the portable executable, runs the tests against it, and lints the API schema with `go tool '-modfile=tools/go.mod' buf lint`.

To check layouts without driving the mouse, build with `-tags qa` and set `SPACE_REVIEW_QA` before launching, for example `view=files;theme=light;row=2`. Use `scope` to open a saved location, `window=min` for the minimum window size, and `map=1` or `filters=1` to open those views. The `coverage`, `export`, `schedule`, and `duplicates` keys open the corresponding dialogs. Release builds leave this out.

Set `SPACE_REVIEW_EXE` to the absolute path to a built Windows executable to run the portable MCP tests. They exercise real file changes across restart, concurrent captures and shared logs, and continued operation when logging is disabled or unavailable. Set `SPACE_REVIEW_TEST_LIVE_DOCKER=1` to also verify usage and timings against the local Docker engine.

Set `SPACE_REVIEW_TEST_SCHEDULER=1` to test registration, lookup, and removal of a temporary Windows task. With `SPACE_REVIEW_EXE` set, the same test also starts the scheduled executable and checks its saved result.
