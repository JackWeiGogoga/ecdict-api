package tts

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SweepCache deletes the least recently used cache files (by mtime) until the cache is at most
// 90% of CacheMaxBytes. It returns the number of files removed. A zero cap disables it.
func (s *Service) SweepCache() (int, error) {
	if s.cacheMaxBytes <= 0 {
		return 0, nil
	}
	type entry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var (
		files []entry
		total int64
	)
	staleTmp := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(s.cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			if info.ModTime().Before(staleTmp) {
				_ = os.Remove(path)
			}
			return nil
		}
		files = append(files, entry{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil || total <= s.cacheMaxBytes {
		return 0, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	target := s.cacheMaxBytes / 10 * 9
	removed := 0
	for _, f := range files {
		if total <= target {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
			removed++
		}
	}
	return removed, nil
}

// RunCacheJanitor sweeps the cache every interval until ctx is cancelled.
func (s *Service) RunCacheJanitor(ctx context.Context, interval time.Duration) {
	if s.cacheMaxBytes <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := s.SweepCache(); err != nil {
			slog.Error("tts cache sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("tts cache sweep", "removed_files", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
