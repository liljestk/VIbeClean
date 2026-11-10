package local

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
)

// ErrCaptureBusy is returned to scheduled runs that overlap another capture.
var ErrCaptureBusy = model.Conflict("another scan is running")

func (s *Service) lockCapture(ctx context.Context, wait bool) (func(), error) {
	file, err := os.OpenFile(s.indexPath+".capture.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(file.Fd())
	var region windows.Overlapped
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &region)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			file.Close()
			return nil, err
		}
		if !wait {
			file.Close()
			return nil, ErrCaptureBusy
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
