# Space Review

Space Review is a Windows x64 executable with a GPU-drawn desktop UI built with Gio, SQLite, and a stdio MCP server. The index defaults to %LOCALAPPDATA%\SpaceReview\index.sqlite3. To keep the database with a portable copy, launch either mode with --index followed by the local database path.

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

## Labels and cleanup

Every measured file counts toward one category, and every category has a removal level:

- **Safe to clear** · caches and logs, build output and installed dependencies, package manager caches, Windows temporary files and the Recycle Bin. Apps and tools recreate them, or they were already deleted.
- **Check first** · downloads, backups, checkouts and worktrees, scratch folders, virtual machine and emulator disks, Docker storage and the previous Windows installation.
- **Uninstall** · programs and games. Remove them in Settings > Apps or in their launcher, because deleting the folder leaves registrations behind.
- **Keep** · Windows, app data, projects and personal files.

Labels come from where Windows, installers and launchers put files: Program Files and per-user program folders, Steam, Epic, GOG, Ubisoft, EA, Riot and Xbox libraries, profile folders, `AppData\Local\Temp` and package manager caches. A folder's own listing can relabel it. A `.git`, `.hg` or `.svn` folder or a project manifest marks a project, a `CACHEDIR.TAG` file marks a cache, and launcher manifests or a Unity runtime mark a game. Windows, installed software, caches, generated output and backups keep their label for everything inside them, while a project's `node_modules` is still build output. Launcher rules do not apply inside app data, where launchers keep settings and overlays. [Cache directory tagging](https://bford.info/cachedir/)

Captures record a total per category, the top folder of each cleanup category as a review candidate, and the top folder of each program, game and project. A labeled folder inside another category's folder counts only toward its own, so a Steam game is not part of Steam's size. The Files summary leads with Safe to clear, Check first and Uninstall. Each opens the matching items, largest first, and the removal filter narrows any view. The labels are estimates, so the inspector says why a folder got its label.

Captures store the version of the labeling rules. Review candidate and labeled folder comparisons start once both captures use the same rules. Category totals are not compared because they span drives and unreadable locations that can differ between captures. The walk reads a folder's whole listing before counting its files and decodes only the short file names that can act as markers. A scan of 3.35 million files in a user profile took 35 to 36 seconds before and after labeling.

## MCP

The MCP view connects assistants to the same saved history. Storage inspection uses read-only filesystem metadata and local Docker API requests. Tool results can include file paths, sizes, and scan times. A capture saves a snapshot to the index. [MCP stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)

- **Claude Desktop** · Save the `.mcpb` extension and open it in Claude Desktop to install. The extension bundles the current Space Review executable. Export it again after upgrading Space Review or switching history locations. [Claude Desktop extensions](https://support.claude.com/en/articles/10949351-getting-started-with-local-mcp-servers-on-claude-desktop)
- **Local Codex or Claude Code** · Copy the matching setup command and run it in PowerShell with that CLI installed. Start a new local chat and check `/mcp`. In the ChatGPT desktop app, you can also add the command and arguments as a STDIO server under Settings → MCP servers. [Codex MCP setup](https://learn.chatgpt.com/docs/extend/mcp#connect-codex-to-an-mcp-server) · [Claude Code MCP setup](https://code.claude.com/docs/en/mcp#option-3-add-a-local-stdio-server)
- **ChatGPT web** · Use the command and arguments from Copy stdio JSON as the Secure MCP Tunnel's stdio target. In ChatGPT Plugins, choose Add custom MCP server, then Tunnel. The PC and tunnel client must be running, and the account must have access to the tunnel and custom MCP servers. [OpenAI Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels)

Ask “Use Space Review to show my ten largest files and the folders using the most space.” The answer should include the saved scan time. For a coding session, scan the same project folder before and after. Ask “What grew most between those scans?” `storage_changes` with `days=0` compares the last two captures of the same scope. Check the two capture times against the session. After scanning a drive, ask “What can I safely clean up on C:?” `storage_breakdown` totals the capture by category and removal level and lists the largest review candidates and the largest programs, games and projects.

## Ask assistant

Files, Changes, and Docker can prepare an editable question with up to ten of the largest measured rows from the current filtered view. The draft includes display names, sizes, scan times, and coverage limits. Choose ChatGPT desktop or Claude to prefill a new chat using the installed app's sign-in. ChatGPT web and desktop launch fallbacks copy the prompt for pasting. Sending the draft happens in the assistant. This flow works with saved scans independently of the MCP connection. [ChatGPT desktop links](https://learn.chatgpt.com/docs/app/commands#deep-links) · [Claude Desktop links](https://support.claude.com/en/articles/14729294-open-claude-desktop-with-a-link)

## Desktop interaction

Changes opens first. The sidebar holds navigation and drive capacity. The workspace gives the ranked results most of the width and separates drive usage from changes in comparable folders. Tone and spacing distinguish the panes. The inspector puts the selected item and its actions above the ranking chart. [Little Snitch Network Monitor](https://help.obdev.at/littlesnitch6/lsm-overview) · [Cohesity Data Cloud](https://ux-design-awards.com/winners/2026-2-cohesity-data-cloud) · [beQ Dashboard](https://ux-design-awards.com/winners/2026-1-beq-dashboard-smart-investment-reward-control) · [Apple split views](https://developer.apple.com/design/human-interface-guidelines/split-views)

Each review view has one capture action in the toolbar. The shared status strip keeps progress and cancellation available while saved results stay visible. Switching a location loads that location’s saved scan, and returning from folder inspection restores the previous search and selection. [Apple toolbars](https://developer.apple.com/design/human-interface-guidelines/toolbars) · [Feedback](https://developer.apple.com/design/human-interface-guidelines/feedback)

The window is drawn with Gio on the GPU, so the workspace matches the concept instead of the classic Windows controls. The title bar, palette, and focus colors follow the Windows light, dark, and high contrast settings. Folder and save choices use the Windows common item dialogs. [Gio](https://gioui.org/)

The layout adapts to the window. Controls size themselves to their labels, so text is shortened only when the window is too small for it. Below 900 dp the sidebar becomes an icon rail. Below 680 dp the inspector moves under the results, and short windows give its space back to the table. The summary card wraps into balanced rows and turns denser on short windows, and filters wrap below the search field. Narrow tables hide Before, Modified, or Activity before shortening values.

Tab moves keyboard focus with a visible ring. In the table, the arrow, Page Up, Page Down, Home, and End keys move the selection, Enter opens the selected item, and Ctrl+C copies its path. F5 captures the current location or refreshes Docker, Ctrl+F focuses search, and Ctrl+I toggles the inspector. Signed amounts and text communicate changes alongside the bars. Gio does not expose UI Automation on Windows yet, so screen readers such as Narrator cannot read the window. [Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility)

The window opens while the index loads. Index reads, drive probes, and Explorer launches run on workers. New selections cancel older reads, and sorting preserves the selected item's identity. Search reuses the current view's prepared rows and sorts a list of visible row indexes.

## Diagnostics

Select Diagnostics at the bottom of the sidebar. The desktop and MCP share newline-delimited JSON logs in the diagnostics folder beside the selected index. The default is %LOCALAPPDATA%\SpaceReview\diagnostics. The active log and one previous log each have a 2 MiB limit. A Windows byte-range lock coordinates appends and rotation across processes. [Windows file locking](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)

Group records by `session` and `op` to follow a capture or comparison. `scan.files` separates discovery, traversal, and finalization and counts files, metadata batches, fallback reads, and coverage gaps. `index.save` records writer wait, version reuse, and commit time. `index.compare` separates SQLite reads from change matching. `docker.api` records response status, bytes, and header and decode times. `mcp.request` times requests. `gui.compare` and `gui.capture` include the UI queue and display update times. Durations are in milliseconds.

Logging is local and enabled at info level. Launch either mode with `--log-level debug` for fast UI render timings, or `--log-level off` to disable logging. Slow renders are logged at info settings. Error codes and sanitized Go panic stacks help locate bugs. Logs omit file paths, search text, MCP arguments, Docker names, and raw error messages. The bounded queue keeps filesystem writes off the UI thread and reports dropped records when it can resume writing.

## Testing

Run `go test -count=1 ./...` and `go vet ./...` in this folder. The tests cover Windows allocation and hard links, SQLite history, concurrent access and rollback, Docker usage parsing, and diagnostics rotation, contention, error reporting, and privacy.

To check layouts without driving the mouse, build with `-tags qa` and set `SPACE_REVIEW_QA` before launching, for example `view=files;theme=light;row=2`. The window opens in that state once history loads. Other keys open a drop-down, the assistant draft, an alert, or a simulated capture. Release builds leave this out.

Set `SPACE_REVIEW_EXE` to the absolute path to a built Windows executable to run the portable MCP tests. They exercise real file changes across restart, concurrent captures and shared logs, and continued operation when logging is disabled or unavailable. Set `SPACE_REVIEW_TEST_LIVE_DOCKER=1` to also verify usage and timings against the local Docker engine.
