package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDockerGroupsSeparateSharedAndPartialUsage(t *testing.T) {
	var data dockerData
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
	rows := make(map[rowKey]record)
	for _, item := range dockerRecords(data) {
		rows[rowKey{item.Series, item.Key}] = item
	}
	for _, want := range []struct {
		key     rowKey
		bytes   int64
		partial bool
	}{
		{rowKey{projects, sharedGroup}, 40, false},
		{rowKey{projects, "alpha"}, 10, true},
		{rowKey{projects, "beta"}, 20, false},
		{rowKey{projects, builderGroup}, 81, false},
		{rowKey{dockerSum, volumes}, 120, true},
		{rowKey{dockerSum, images}, 100, false},
		{rowKey{dockerSum, buildCache}, 50, false},
	} {
		got, ok := rows[want.key]
		if !ok || got.Bytes != want.bytes || got.Partial != want.partial {
			t.Fatalf("%+v = %+v, want %+v", want.key, got, want)
		}
	}
	old := record{Series: projects, Key: "alpha", Bytes: 200}
	alpha := rowKey{projects, "alpha"}
	changes, omitted, err := calculateChanges(context.Background(), map[rowKey]record{alpha: old}, map[rowKey]record{alpha: rows[alpha]},
		coverage{}, coverage{})
	if err != nil || len(changes) != 0 || omitted != 1 {
		t.Fatalf("partial sizes should not produce a false drop: %+v, omitted %d, err = %v", changes, omitted, err)
	}
}
