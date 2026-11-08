package desktop

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/model"
)

// displayRow is one prepared table row. Rank orders the bars; Before and
// After hold sizes for sorting, or -1 when the column has no size.
type displayRow struct {
	Item   model.Record
	Cells  [4]string
	Rank   int64
	Before int64
	After  int64
	Detail string
}

// tableModel shows a filtered, sorted list of indexes into prepared rows, so
// searching and sorting never rebuild the rows.
type tableModel struct {
	walk.TableModelBase
	walk.SorterBase
	items []displayRow
	rows  []int
}

func (m *tableModel) RowCount() int { return len(m.rows) }

func (m *tableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) || col < 0 || col >= len(displayRow{}.Cells) {
		return ""
	}
	return m.items[m.rows[row]].Cells[col]
}

func (m *tableModel) ID(index int) any {
	if index < 0 || index >= len(m.rows) {
		return nil
	}
	item := m.items[m.rows[index]].Item
	return model.RowKey{Series: item.Series, Key: item.Key}
}

func (m *tableModel) Sort(column int, order walk.SortOrder) error {
	if column < 0 || column >= len(displayRow{}.Cells) {
		return m.SorterBase.Sort(column, order)
	}
	slices.SortFunc(m.rows, func(left, right int) int {
		ordering := compareColumn(&m.items[left], &m.items[right], column)
		if ordering == 0 {
			if byName := strings.Compare(m.items[left].Item.Name, m.items[right].Item.Name); byName != 0 {
				return byName
			}
			return cmp.Compare(left, right)
		}
		if order == walk.SortDescending {
			return -ordering
		}
		return ordering
	})
	return m.SorterBase.Sort(column, order)
}

func compareColumn(a, b *displayRow, column int) int {
	switch {
	case column == 1:
		return cmp.Compare(a.Rank, b.Rank)
	case column == 2 && (a.Before >= 0 || b.Before >= 0):
		return cmp.Compare(a.Before, b.Before)
	case column == 3 && (a.After >= 0 || b.After >= 0):
		return cmp.Compare(a.After, b.After)
	}
	return strings.Compare(strings.ToLower(a.Cells[column]), strings.ToLower(b.Cells[column]))
}

// rankedPane is a table with a bar chart of its largest rows, a detail box
// and actions for the selected row.
type rankedPane struct {
	ctx            context.Context
	model          tableModel
	table          *walk.TableView
	empty          *walk.Label
	chartHeading   *walk.Label
	detail         *walk.TextEdit
	bars           [5]*walk.ProgressBar
	barLabels      [5]*walk.Label
	action         *walk.PushButton
	copy           *walk.PushButton
	inspect        *walk.PushButton
	projectAction  bool
	inspectSeries  string
	inspectReady   bool
	inspectBlocked bool
	sorted         bool
	emptyText      string
}

func (p *rankedPane) layout(tableName, chartTitle string, columns [4]string, openText, inspectText string,
	projectAction bool, open, inspect func()) d.Widget {
	p.projectAction = projectAction
	chart := []d.Widget{
		d.Label{AssignTo: &p.chartHeading, Text: chartTitle, Font: d.Font{Bold: true}},
	}
	for slot := range p.bars {
		chart = append(chart,
			d.Label{AssignTo: &p.barLabels[slot], Text: " ", EllipsisMode: d.EllipsisEnd, NoPrefix: true},
			d.ProgressBar{AssignTo: &p.bars[slot], MaxValue: 1000, MinSize: d.Size{Height: 7}, MaxSize: d.Size{Height: 7}},
		)
	}
	chart = append(chart,
		d.Label{Text: "Details", Font: d.Font{Bold: true}},
		d.TextEdit{AssignTo: &p.detail, ReadOnly: true, VScroll: true,
			Accessibility: d.Accessibility{Name: "Selected item details"},
			MinSize:       d.Size{Height: 85}, Text: selectPrompt},
	)
	buttons := []d.Widget{
		d.PushButton{AssignTo: &p.action, Text: openText, OnClicked: open, Enabled: false},
		d.PushButton{AssignTo: &p.copy, Text: "Copy", Enabled: false, OnClicked: p.copySelection},
	}
	if inspect != nil {
		buttons = append(buttons, d.PushButton{AssignTo: &p.inspect, Text: inspectText, OnClicked: inspect, Enabled: false})
	}
	chart = append(chart, d.Composite{Layout: d.HBox{MarginsZero: true}, Children: buttons})
	return d.Composite{Layout: d.HBox{MarginsZero: true, Spacing: 10}, Children: []d.Widget{
		d.Composite{StretchFactor: 1, Layout: d.VBox{MarginsZero: true, Spacing: 6}, Children: []d.Widget{
			d.Label{AssignTo: &p.empty, Visible: false, NoPrefix: true, EllipsisMode: d.EllipsisEnd,
				Font: d.Font{Bold: true}},
			d.TableView{
				AssignTo: &p.table, Model: &p.model, AlternatingRowBG: true, StretchFactor: 1,
				Accessibility:       d.Accessibility{Name: tableName},
				LastColumnStretched: true, SelectionHiddenWithoutFocus: false,
				Columns: []d.TableViewColumn{
					{Title: columns[0], Width: 260},
					{Title: columns[1], Width: 108, Alignment: d.AlignFar},
					{Title: columns[2], Width: 115},
					{Title: columns[3], Width: 122},
				},
				OnCurrentIndexChanged: p.showDetail,
				OnItemActivated: func() {
					if p.action != nil && p.action.Enabled() {
						open()
					}
				},
			},
		}},
		d.Composite{Layout: d.VBox{MarginsZero: true, Spacing: 7}, Children: chart,
			MinSize: d.Size{Width: 316}, MaxSize: d.Size{Width: 328}},
	}}
}

func (p *rankedPane) copySelection() {
	row, ok := p.selected()
	if !ok {
		return
	}
	value := row.Item.Path
	if value == "" {
		value = row.Item.Name
	}
	if err := walk.Clipboard().SetText(value); err != nil {
		diag.Log(p.ctx, slog.LevelWarn, "gui.copy_failed", slog.String("error_kind", diag.ErrorKind(err)))
		_ = p.detail.SetText(row.Detail + "\r\n\r\nCould not copy. Try again.")
		return
	}
	_ = p.copy.SetText("Copied")
}

func (p *rankedPane) selected() (displayRow, bool) {
	if p.table != nil {
		if index := p.table.CurrentIndex(); index >= 0 && index < len(p.model.rows) {
			return p.model.items[p.model.rows[index]], true
		}
	}
	return displayRow{}, false
}

func (p *rankedPane) restoreSelection(key model.RowKey) {
	for index, source := range p.model.rows {
		row := &p.model.items[source]
		if row.Item.Series == key.Series && row.Item.Key == key.Key {
			_ = p.table.SetCurrentIndex(index)
			return
		}
	}
}

func (p *rankedPane) showDetail() {
	if p.detail == nil || p.action == nil || p.copy == nil {
		return
	}
	row, ok := p.selected()
	p.copy.SetEnabled(ok)
	_ = p.copy.SetText("Copy")
	p.action.SetEnabled(ok && ((!p.projectAction && row.Item.Path != "") || (p.projectAction && row.Item.Series == model.Projects)))
	if p.inspect != nil {
		p.inspect.SetEnabled(ok && row.Item.Directory && !p.inspectBlocked &&
			(p.inspectSeries == "" || (p.inspectReady && row.Item.Series == p.inspectSeries)))
	}
	text := selectPrompt
	if ok {
		text = row.Detail
	}
	_ = p.detail.SetText(text)
}

func (p *rankedPane) show(rows []displayRow, query, empty string) {
	if p.table == nil {
		return
	}
	previous, keep := p.selected()
	p.model.items, p.emptyText = rows, empty
	p.applyFilter(query, previous, keep)
}

func (p *rankedPane) search(box *walk.LineEdit, view string) {
	if p.table == nil || box == nil || len(p.model.items) == 0 {
		return
	}
	started := time.Now()
	previous, keep := p.selected()
	p.applyFilter(box.Text(), previous, keep)
	logRender(p.ctx, view+".search", started, len(p.model.items), len(p.model.rows))
}

func (p *rankedPane) applyFilter(query string, previous displayRow, keep bool) {
	filter := model.ParseFilter(query)
	rows := p.model.rows[:0]
	for index := range p.model.items {
		row := &p.model.items[index]
		if filter.Matches(row.Item, row.Detail) {
			rows = append(rows, index)
		}
	}
	p.model.rows = rows
	empty := p.emptyText
	if len(rows) == 0 && len(p.model.items) > 0 {
		empty = "No matches. Try a different search."
	}
	_ = p.empty.SetText(empty)
	p.empty.SetVisible(len(rows) == 0)
	column, order := p.model.SortedColumn(), p.model.SortOrder()
	if !p.sorted || column < 0 {
		column, order = 1, walk.SortDescending
		p.sorted = true
	}
	_ = p.model.Sort(column, order)
	p.model.PublishRowsReset()
	_ = p.table.SetCurrentIndex(p.selectionAfterFilter(previous, keep))
	p.showDetail()
	p.drawBars(rows)
}

func (p *rankedPane) selectionAfterFilter(previous displayRow, keep bool) int {
	if len(p.model.rows) == 0 {
		return -1
	}
	if keep {
		for index, source := range p.model.rows {
			row := &p.model.items[source]
			if row.Item.Series == previous.Item.Series && row.Item.Key == previous.Item.Key {
				return index
			}
		}
	}
	return 0
}

// drawBars shows the five largest ranked rows as bars scaled to the largest.
func (p *rankedPane) drawBars(rows []int) {
	var ranked [len(p.bars)]int
	count := 0
	for _, source := range rows {
		rank := p.model.items[source].Rank
		if rank <= 0 {
			continue
		}
		slot := count
		for slot > 0 && p.model.items[ranked[slot-1]].Rank < rank {
			slot--
		}
		if slot < len(ranked) {
			count = min(count+1, len(ranked))
			copy(ranked[slot+1:count], ranked[slot:count-1])
			ranked[slot] = source
		}
	}
	p.chartHeading.SetVisible(count != 0)
	for slot, bar := range p.bars {
		text, value := " ", 0
		visible := slot < count
		if visible {
			row := p.model.items[ranked[slot]]
			text = row.Cells[1] + "   " + row.Item.Name
			value = max(1, int(float64(row.Rank)*1000/float64(p.model.items[ranked[0]].Rank)))
		}
		_ = p.barLabels[slot].SetText(text)
		bar.SetValue(value)
		p.barLabels[slot].SetVisible(visible)
		bar.SetVisible(visible)
	}
}

func logRender(ctx context.Context, view string, started time.Time, input, shown int) {
	elapsed := time.Since(started)
	level := slog.LevelDebug
	if elapsed >= slowRender {
		level = slog.LevelWarn
	}
	diag.Log(ctx, level, "gui.render", slog.String("view", view),
		slog.Float64("duration_ms", diag.Milliseconds(elapsed)), slog.Int("input_rows", input), slog.Int("visible_rows", shown))
}
