package local

import (
	"bufio"
	"context"
	"encoding/gob"
	"io"
	"os"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/scan"
)

func (s *Service) Duplicates(ctx context.Context, captureID int64, folder string) (model.DuplicateResult, error) {
	// Copy candidate metadata before hashing so a long content read does not
	// hold a SQLite read transaction or prevent a concurrent capture.
	spool, err := os.CreateTemp("", "space-review-duplicates-*.tmp")
	if err != nil {
		return model.DuplicateResult{}, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	writer := bufio.NewWriterSize(spool, 256*1024)
	encoder := gob.NewEncoder(writer)
	offline, err := s.index.DuplicateCandidates(ctx, captureID, folder, func(item model.InventoryEntry) error {
		return encoder.Encode(&item)
	})
	if err != nil {
		return model.DuplicateResult{}, err
	}
	if err := writer.Flush(); err != nil {
		return model.DuplicateResult{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return model.DuplicateResult{}, err
	}
	decoder := gob.NewDecoder(bufio.NewReaderSize(spool, 256*1024))
	result, err := scan.VerifyDuplicates(ctx, func() (model.InventoryEntry, error) {
		var item model.InventoryEntry
		err := decoder.Decode(&item)
		return item, err
	})
	result.Offline = offline
	return result, err
}
