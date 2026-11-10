package ui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/liljestk/VIbeClean/internal/model"
)

func TestRecycleRejectsPermanentDelete(t *testing.T) {
	sink := recycleSink{}
	if hr := sink.preDelete(0, &comObject{}); hr != hresultAbort || !errors.Is(sink.err, errRecycleDenied) {
		t.Fatalf("expected the shell operation to abort, got %#x and %v", hr, sink.err)
	}
}

func TestRecycleRejectsStaleFileMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changed.txt")
	if err := os.WriteFile(path, []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	item := model.Record{Series: model.LargeFiles, Path: path, Logical: 7}
	if _, err := recycleIdentityAt(item); err != nil {
		t.Fatal(err)
	}
	item.Logical++
	if _, err := recycleIdentityAt(item); !errors.Is(err, errRecycleChanged) {
		t.Fatalf("expected changed file to be refused, got %v", err)
	}
}
