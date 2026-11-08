package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/diag/diagtest"
	"github.com/liljestk/VIbeClean/internal/model"
)

func TestGroupsSeparateSharedAndPartialUsage(t *testing.T) {
	var data usage
	err := json.Unmarshal([]byte(`{
		"LayersSize": 900,
		"ImageUsage": {"TotalSize": 100, "Reclaimable": 30, "Items": [{"Id":"img","Size":120,"SharedSize":20}]},
		"Containers": [
			{"Id":"a","Names":["/alpha"],"SizeRw":10,"Labels":{"com.docker.compose.project":"alpha"},
			 "Mounts":[{"Type":"volume","Name":"shared"},{"Type":"volume","Name":"unknown"}]},
			{"Id":"b","Names":["/beta"],"SizeRw":20,"Labels":{"com.docker.compose.project":"beta"},
			 "Mounts":[{"Type":"volume","Name":"shared"}]},
			{"Id":"c","Names":["/buildx_buildkit_example0"],"SizeRw":1,
			 "Mounts":[{"Type":"volume","Name":"buildx_buildkit_example0_state"}]}
		],
		"Volumes": [
			{"Name":"shared","UsageData":{"Size":40,"RefCount":2}},
			{"Name":"unknown"},
			{"Name":"buildx_buildkit_example0_state","UsageData":{"Size":80,"RefCount":1}}
		],
		"BuildCacheUsage":{"TotalSize":50,"Reclaimable":15}
	}`), &data)
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[model.RowKey]model.Record)
	for _, item := range records(data) {
		rows[model.RowKey{Series: item.Series, Key: item.Key}] = item
	}
	for _, want := range []struct {
		key     model.RowKey
		bytes   int64
		partial bool
	}{
		{model.RowKey{Series: model.Projects, Key: sharedGroup}, 40, false},
		{model.RowKey{Series: model.Projects, Key: "alpha"}, 10, true},
		{model.RowKey{Series: model.Projects, Key: "beta"}, 20, false},
		{model.RowKey{Series: model.Projects, Key: builderGroup}, 81, false},
		{model.RowKey{Series: model.DockerTotals, Key: model.Volumes}, 120, true},
		{model.RowKey{Series: model.DockerTotals, Key: model.Images}, 100, false},
		{model.RowKey{Series: model.DockerTotals, Key: model.BuildCache}, 50, false},
		{model.RowKey{Series: model.Images, Key: "img"}, 120, false},
	} {
		got, ok := rows[want.key]
		if !ok || got.Bytes != want.bytes || got.Partial != want.partial {
			t.Fatalf("%+v = %+v, want %+v", want.key, got, want)
		}
	}
	if !strings.Contains(rows[model.RowKey{Series: model.Images, Key: "img"}].Detail, "Unique size: 100 B") {
		t.Fatal("image details should show the unique size")
	}
}

func TestUniquePipesKeepsFirstOfEachName(t *testing.T) {
	got := unique([]string{"docker_engine", "dockerDesktopLinuxEngine", "Docker_Engine"})
	if len(got) != 2 || got[0] != "docker_engine" || got[1] != "dockerDesktopLinuxEngine" {
		t.Fatalf("unique pipes = %v", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPIDiagnostics(t *testing.T) {
	ctx, dir, closeLog, err := diag.Open(context.Background(), filepath.Join(t.TempDir(), "index.sqlite3"), "test", "info")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	ctx = diag.Operation(ctx)
	for _, tc := range []struct {
		status int
		body   string
		failed bool
	}{
		{http.StatusOK, "{\"api\":\"local\"}", false},
		{http.StatusServiceUnavailable, diagtest.PrivateMarker, true},
		{http.StatusOK, diagtest.PrivateMarker, true},
	} {
		client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/system/df" {
				t.Fatalf("unexpected request: %v %v", r.Method, r.URL)
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}
		var output map[string]string
		if err := getJSON(ctx, client, "/system/df", &output); (err != nil) != tc.failed {
			t.Fatalf("unexpected Docker request result: %v", err)
		}
	}
	closeLog()
	events, raw := diagtest.ReadEvents(t, dir)
	if strings.Contains(raw, diagtest.PrivateMarker) || len(events) != 3 {
		t.Fatalf("unexpected diagnostic records: %+v", events)
	}
	for _, event := range events {
		if event.Message != "docker.api" || event.Endpoint != "/system/df" || event.Duration == nil {
			t.Fatalf("missing API diagnostics: %+v", event)
		}
	}
	if events[0].Status != 200 || events[0].Result != "ok" || events[0].ResponseBytes == 0 ||
		events[1].Status != 503 || events[1].Result != "error" || events[2].ErrorKind != "invalid_json" {
		t.Fatalf("unexpected API results: %+v", events)
	}
}
