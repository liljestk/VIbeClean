package desktop

import (
	"encoding/json"
	"strings"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"
)

// mcpPage shows the configuration an MCP client needs to start this
// executable as an MCP server over the same history.
func (u *gui) mcpPage() (d.TabPage, error) {
	config, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"space-review": map[string]any{"command": u.opts.MCPCommand, "args": u.opts.MCPArgs},
	}}, "", "  ")
	if err != nil {
		return d.TabPage{}, err
	}
	text := strings.ReplaceAll(string(config), "\n", "\r\n")
	var copyStatus *walk.Label
	return d.TabPage{Title: "MCP", Layout: d.VBox{}, Children: []d.Widget{
		d.Label{Text: "Connect a local MCP client", Font: d.Font{PointSize: 16, Bold: true}},
		d.TextLabel{Text: "Copy the configuration into your MCP client. It launches this executable and reads the same saved history.",
			MinSize: d.Size{Width: 440}},
		d.Composite{Layout: d.HBox{MarginsZero: true}, Children: []d.Widget{
			d.PushButton{Text: "Copy configuration", OnClicked: func() {
				message := "Configuration copied."
				if err := walk.Clipboard().SetText(text); err != nil {
					message = "Could not copy. Select the configuration to copy it manually."
				}
				_ = copyStatus.SetText(message)
			}},
			d.Label{AssignTo: &copyStatus, NoPrefix: true},
			d.HSpacer{},
		}},
		d.TextEdit{ReadOnly: true, Text: text, Font: d.Font{Family: "Consolas", PointSize: 10},
			Accessibility: d.Accessibility{Name: "MCP configuration"},
			MinSize:       d.Size{Height: 165}, VScroll: true, HScroll: true},
		d.Label{Text: "Tools", Font: d.Font{Bold: true}},
		d.TextLabel{Text: "list_storage_scopes, list_drives, largest_storage_items, storage_changes, capture_storage", MinSize: d.Size{Width: 440}},
		d.Label{Text: "History", Font: d.Font{Bold: true}},
		d.TextLabel{Text: u.opts.Location, NoPrefix: true, MinSize: d.Size{Width: 440}},
		d.VSpacer{},
	}}, nil
}
