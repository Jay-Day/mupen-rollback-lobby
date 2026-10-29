package config

import (
	"context"
	"os"
	"time"
)

// fileStamp identifies one version of a file well enough to notice edits.
type fileStamp struct {
	exists  bool
	size    int64
	modTime int64
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, size: fi.Size(), modTime: fi.ModTime().UnixNano()}
}

// WatchFile calls onChange whenever the file at path is created, modified or
// removed, checking every interval until ctx is done. Polling keeps it
// portable and still sees editors that replace the file instead of writing it
// in place.
func WatchFile(ctx context.Context, path string, interval time.Duration, onChange func()) {
	last := stampOf(path)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cur := stampOf(path); cur != last {
				last = cur
				onChange()
			}
		}
	}
}
