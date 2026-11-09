package ui

import (
	"encoding/json"
	"errors"
	"image"
	"path/filepath"
	"strings"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"golang.org/x/sys/windows"
)

const tunnelGuide = "https://developers.openai.com/api/docs/guides/secure-mcp-tunnels"

var powerShellQuotes = strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛")

func mcpSetupCommand(prefix, executable string, args []string) string {
	var command strings.Builder
	command.WriteString(prefix)
	for _, arg := range append([]string{executable}, args...) {
		command.WriteString(" '")
		command.WriteString(powerShellQuotes.Replace(arg))
		command.WriteByte('\'')
	}
	return command.String()
}

// mcpTab holds the assistant setup actions.
type mcpTab struct {
	stdioJSON    string
	codexSetup   string
	claudeSetup  string
	saveBundle   widget.Clickable
	copyCodex    widget.Clickable
	copyClaude   widget.Clickable
	copyJSON     widget.Clickable
	openTunnel   widget.Clickable
	copyLocation widget.Clickable
	saving       bool
}

func (u *ui) initMCP() {
	executable, args := u.opts.MCPCommand, u.opts.MCPArgs
	config, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"space-review": map[string]any{"command": executable, "args": args},
	}}, "", "  ")
	if err == nil {
		u.stdioJSON = string(config)
	}
	u.codexSetup = mcpSetupCommand("codex mcp add space-review --", executable, args)
	u.claudeSetup = mcpSetupCommand("claude mcp add --transport stdio --scope user space-review --", executable, args)
}

func (u *ui) copied(text string) {
	u.copyText(text)
	u.setActivity("Copied to clipboard.", toneReady)
}

func (u *ui) saveClaudeExtension() {
	if u.saving {
		return
	}
	u.saving = true
	hwnd, command, args := u.hwnd, u.opts.MCPCommand, u.opts.MCPArgs
	go func() {
		path, err := saveFileDialog(hwnd, "Save Claude Desktop extension", "Space Review.mcpb", "Claude Desktop extension (*.mcpb)", "*.mcpb", "mcpb")
		if err != nil || path == "" {
			u.post(func() {
				u.saving = false
				if err != nil {
					u.showAlert("Could not open the save dialog", err.Error())
				}
			})
			return
		}
		if !strings.EqualFold(filepath.Ext(path), ".mcpb") {
			path += ".mcpb"
		}
		u.post(func() { u.setActivity("Saving extension…", toneBusy) })
		err = writeClaudeBundle(path, command, args)
		u.post(func() {
			u.saving = false
			if err == nil {
				u.setActivity("Extension saved. Open it in Claude Desktop to install.", toneReady)
				return
			}
			u.setActivity("Extension could not be saved.", toneProblem)
			if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
				err = errors.New("a file with that name already exists. Choose a new file name")
			}
			u.showAlert("Could not save extension", err.Error())
		})
	}()
}

func (u *ui) mcpView(gtx layout.Context) layout.Dimensions {
	t := u.th
	if u.saveBundle.Clicked(gtx) {
		u.saveClaudeExtension()
	}
	if u.copyCodex.Clicked(gtx) {
		u.copied(u.codexSetup)
	}
	if u.copyClaude.Clicked(gtx) {
		u.copied(u.claudeSetup)
	}
	if u.copyJSON.Clicked(gtx) && u.stdioJSON != "" {
		u.copied(u.stdioJSON)
	}
	if u.copyLocation.Clicked(gtx) {
		u.copied(u.opts.Location)
	}
	if u.openTunnel.Clicked(gtx) {
		go func() {
			if err := openShellURL(tunnelGuide); err != nil {
				u.post(func() { u.showAlert("Could not open the setup guide", err.Error()) })
			}
		}()
	}
	cards := []layout.Widget{
		func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, iconSave, "Claude Desktop",
				"Save the extension, open the .mcpb file in Claude Desktop, and select Install. Save it again after upgrading Space Review or switching history.",
				nil,
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.saveBundle, buttonStyle{kind: primary, icon: iconSave, label: "Save Claude extension…", disabled: u.saving})
				})
		},
		func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, iconTerminal, "Codex and Claude Code",
				"With the CLI installed, paste its setup command into PowerShell, then start a new local chat and check /mcp.",
				nil,
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.copyCodex, buttonStyle{icon: iconCopy, label: "Copy Codex command"})
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.copyClaude, buttonStyle{icon: iconCopy, label: "Copy Claude Code command"})
				})
		},
		func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, iconCode, "ChatGPT web and other clients",
				"Use the stdio JSON as the target of an OpenAI Secure MCP Tunnel, or add the command and arguments as a stdio server in other assistants.",
				nil,
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.copyJSON, buttonStyle{icon: iconCopy, label: "Copy stdio JSON", disabled: u.stdioJSON == ""})
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.openTunnel, buttonStyle{kind: quiet, icon: iconExternal, label: "Tunnel setup guide"})
				})
		},
		func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, iconMessage, "Try asking", "", func(gtx layout.Context) layout.Dimensions {
				quote := func(text string) layout.Widget {
					return func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return t.panel(gtx, layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, 8, t.well, func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							return t.label(gtx, t.body(t.ink).wrapped(-1), text)
						})
					}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(quote("“Use Space Review to show my ten largest files and the folders using the most space.”")),
					layout.Rigid(space(0, 8)),
					layout.Rigid(quote("Scan a project folder before and after coding, then ask “What grew most between those scans?”")),
				)
			})
		},
		func(gtx layout.Context) layout.Dimensions {
			return u.card(gtx, iconHistory, "History",
				"Assistants read the same saved scans as this app. Results can include file paths, sizes, and scan times.",
				func(gtx layout.Context) layout.Dimensions {
					s := t.small(t.quiet).wrapped(3)
					s.wrap = text.WrapGraphemes
					return t.label(gtx, s, u.opts.Location)
				},
				func(gtx layout.Context) layout.Dimensions {
					return t.button(gtx, &u.copyLocation, buttonStyle{kind: quiet, icon: iconCopy, label: "Copy location", compact: true})
				})
		},
	}
	return scrollColumn(gtx, &u.mcpList, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(-1),
					"Connect a local assistant to your saved scans. The assistant starts Space Review in MCP mode on this PC; nothing is uploaded until you send a chat.")
			}),
			layout.Rigid(space(0, 16)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return cardGrid(gtx, 360, 14, cards...) }),
			layout.Rigid(space(0, 12)),
		)
	})
}

// card is a titled section on a chrome surface.
func (u *ui) card(gtx layout.Context, ic icon, title, body string, extra layout.Widget, actions ...layout.Widget) layout.Dimensions {
	t := u.th
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return t.panel(gtx, layout.UniformInset(18), 10, t.chrome, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		children := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return drawIcon(gtx, ic, 18, t.blue) }),
					layout.Rigid(space(9, 0)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return t.label(gtx, t.strong(15, t.ink), title) }),
				)
			}),
		}
		if body != "" {
			children = append(children, layout.Rigid(space(0, 8)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return t.label(gtx, t.body(t.quiet).wrapped(-1), body)
			}))
		}
		if extra != nil {
			children = append(children, layout.Rigid(space(0, 10)), layout.Rigid(extra))
		}
		if len(actions) > 0 {
			children = append(children, layout.Rigid(space(0, 14)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return flow{gap: 8, rowGap: 8}.Layout(gtx, actions...)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// cardGrid places cards in as many columns of at least minWidth as fit,
// filling each column in turn so cards keep their natural height.
func cardGrid(gtx layout.Context, minWidth, gap float32, cards ...layout.Widget) layout.Dimensions {
	w := gtx.Constraints.Max.X
	g := gtx.Dp(unit.Dp(gap))
	cols := max(1, min(len(cards), (w+g)/(gtx.Dp(unit.Dp(minWidth))+g)))
	colW := (w - g*(cols-1)) / cols
	heights := make([]int, cols)
	for _, card := range cards {
		col := 0
		for c := range heights {
			if heights[c] < heights[col] {
				col = c
			}
		}
		cgtx := gtx
		cgtx.Constraints = layout.Constraints{Min: image.Pt(colW, 0), Max: image.Pt(colW, 1<<20)}
		r := record(cgtx, card)
		stack := op.Offset(image.Pt(col*(colW+g), heights[col])).Push(gtx.Ops)
		r.call.Add(gtx.Ops)
		stack.Pop()
		heights[col] += r.size.Y + g
	}
	total := 0
	for _, h := range heights {
		total = max(total, h)
	}
	return layout.Dimensions{Size: image.Pt(w, max(0, total-g))}
}
