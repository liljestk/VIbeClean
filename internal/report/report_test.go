package report

import (
	"bytes"
	"encoding/csv"
	"testing"

	"github.com/liljestk/VIbeClean/internal/model"
)

func TestCSVPreservesSignedAmountsAndEscapesText(t *testing.T) {
	r := Report{View: "Changes", Latest: model.Snapshot{Label: "Quick review", Captured: 1000}, Rows: []Row{
		{Item: model.Record{Name: " =formula", Path: `C:\test`, Bytes: 1024}, Change: &Change{Bytes: -1024, Bounded: true}},
	}}
	var file bytes.Buffer
	if err := r.WriteCSV(&file); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&file).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV = %+v, %v", rows, err)
	}
	byName := make(map[string]string)
	for i, col := range rows[0] {
		byName[col] = rows[1][i]
	}
	if byName["name"] != "' =formula" || byName["delta_bytes"] != "-1024" ||
		byName["to_utc"] != "1970-01-01T00:00:01Z" || byName["minimum_magnitude"] != "true" ||
		byName["before_bytes"] != "" {
		t.Fatalf("CSV fields = %+v", byName)
	}
}
