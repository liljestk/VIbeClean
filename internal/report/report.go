// Package report writes a portable copy of the rows a user is reviewing.
package report

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/liljestk/VIbeClean/internal/model"
)

type Report struct {
	View        string          `json:"view"`
	Series      string          `json:"series"`
	Filters     string          `json:"filters,omitempty"`
	Latest      model.Snapshot  `json:"latest"`
	Baseline    *model.Snapshot `json:"baseline,omitempty"`
	ShortWindow bool            `json:"short_window,omitempty"`
	Omitted     int             `json:"omitted_coverage_changes,omitempty"`
	Rows        []Row           `json:"-"`
	// Stream supplies a large, paged view without holding every row in memory.
	Stream func(func(Row) error) error `json:"-"`
	Count  int                         `json:"row_count"`
}

type Row struct {
	Item    model.Record `json:"item"`
	Removal string       `json:"removal,omitempty"`
	Change  *Change      `json:"change,omitempty"`
}

type Change struct {
	Bytes   int64  `json:"delta_bytes"`
	Before  *int64 `json:"before_bytes"`
	After   *int64 `json:"after_bytes"`
	Bounded bool   `json:"minimum_magnitude,omitempty"`
}

func (r Report) WriteJSON(dst io.Writer) error {
	if r.Stream == nil {
		r.Count = len(r.Rows)
	}
	header, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := dst.Write(header[:len(header)-1]); err != nil {
		return err
	}
	if _, err := io.WriteString(dst, ",\"rows\":["); err != nil {
		return err
	}
	first := true
	encoder := json.NewEncoder(dst)
	if err := r.eachRow(func(row Row) error {
		if !first {
			if _, err := io.WriteString(dst, ","); err != nil {
				return err
			}
		}
		first = false
		return encoder.Encode(row)
	}); err != nil {
		return err
	}
	_, err = io.WriteString(dst, "]}\n")
	return err
}

func (r Report) eachRow(emit func(Row) error) error {
	if r.Stream != nil {
		return r.Stream(emit)
	}
	for _, row := range r.Rows {
		if err := emit(row); err != nil {
			return err
		}
	}
	return nil
}

// WriteCSV repeats the capture metadata on each row. An empty view writes
// one metadata-only row, so filtering to zero items still produces a report.
func (r Report) WriteCSV(dst io.Writer) error {
	writer := csv.NewWriter(dst)
	if err := writer.Write([]string{"view", "scope", "series", "filters", "from_utc", "to_utc", "from_coverage", "to_coverage",
		"short_window", "omitted_changes", "name", "path", "category", "removal", "directory", "on_disk_bytes", "logical_bytes",
		"modified_utc", "delta_bytes", "before_bytes", "after_bytes", "minimum_magnitude"}); err != nil {
		return err
	}
	var from, fromCoverage string
	if r.Baseline != nil {
		from, fromCoverage = stamp(r.Baseline.Captured), coverageText(r.Baseline.Coverage)
	}
	fields := []string{
		cell(r.View), cell(r.Latest.Label), cell(r.Series), cell(r.Filters), from, stamp(r.Latest.Captured),
		fromCoverage, coverageText(r.Latest.Coverage), strconv.FormatBool(r.ShortWindow), strconv.Itoa(r.Omitted),
	}
	line := make([]string, len(fields)+12)
	copy(line, fields)
	wrote := false
	if err := r.eachRow(func(row Row) error {
		wrote = true
		item := row.Item
		logical := ""
		if item.Logical > 0 {
			logical = strconv.FormatInt(item.Logical, 10)
		}
		copy(line[len(fields):], []string{
			cell(item.Name), cell(item.Path), cell(item.Kind), cell(row.Removal), strconv.FormatBool(item.Directory),
			bytesText(&item.Bytes), logical, stamp(item.Modified), "", "", "", "",
		})
		if diff := row.Change; diff != nil {
			copy(line[len(line)-4:], []string{strconv.FormatInt(diff.Bytes, 10),
				bytesText(diff.Before), bytesText(diff.After), strconv.FormatBool(diff.Bounded)})
		}
		return writer.Write(line)
	}); err != nil {
		return err
	}
	if !wrote {
		if err := writer.Write(line); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func bytesText(n *int64) string {
	if n == nil || *n < 0 {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

func stamp(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func coverageText(c model.Coverage) string {
	summary := struct {
		Files      int64 `json:"files_checked"`
		Unreadable int   `json:"unreadable_locations"`
		Estimated  int64 `json:"estimated_dirs"`
		Links      int64 `json:"skipped_links"`
		Repeated   int64 `json:"repeated_links"`
		Online     int64 `json:"online_only"`
		Excluded   int   `json:"exclusions"`
	}{c.Checked, len(c.Gaps), c.Estimated, c.Links, c.Shared, c.Online, len(c.Exclusions)}
	text, _ := json.Marshal(summary)
	return string(text)
}

// cell makes text inert when a spreadsheet interprets CSV values. Numeric
// columns are serialized separately, so negative deltas stay numeric.
func cell(value string) string {
	first := strings.TrimLeftFunc(value, unicode.IsSpace)
	r, _ := utf8.DecodeRuneInString(first)
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") ||
		strings.ContainsRune("=+-@＝＋－＠", r) {
		return "'" + value
	}
	return value
}
