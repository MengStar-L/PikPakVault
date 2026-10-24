package aria2

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InsufficientSpaceError is recoverable when another transfer frees its cache.
// Keep byte counts separate from display text so callers need not match strings.
type InsufficientSpaceError struct {
	Needed         uint64
	Available      uint64
	DuringDownload bool
}

func (e *InsufficientSpaceError) Error() string {
	if e.DuringDownload {
		return "aria2 下载过程中磁盘空间不足（错误码 9），缓存已保留"
	}
	return fmt.Sprintf("下载缓存空间不足：至少还需 %d MiB，可用 %d MiB", e.Needed>>20, e.Available>>20)
}

func downloadError(code, reason, dir string, size int64) error {
	if code == "9" {
		var low *InsufficientSpaceError
		if err := CheckSpace(dir, size); errors.As(err, &low) {
			return low
		}
		// Space may have been released since aria2 reported its failure. The
		// persisted waiter will recheck locally before its next resume attempt.
		return &InsufficientSpaceError{DuringDownload: true}
	}
	return fmt.Errorf("aria2 下载失败（错误码 %s），缓存已保留，请重试原任务。%s", code, reason)
}

// CheckSpace only reads local metadata. It can be repeated while a task waits,
// without refreshing credentials or contacting either remote service.
func CheckSpace(dir string, size int64) error {
	if size < 0 {
		return fmt.Errorf("下载文件大小无效")
	}
	if size == 0 {
		return nil
	}
	var allocated int64
	file := filepath.Join(dir, "content")
	if info, err := os.Lstat(file); err == nil {
		if !info.Mode().IsRegular() || info.Size() > size {
			return fmt.Errorf("缓存大小或类型与来源不符")
		}
		var meta metadata
		if readJSON(filepath.Join(dir, "source.json"), &meta) == nil && meta.Complete && meta.Size == size && info.Size() == size {
			if _, err := os.Stat(file + ".aria2"); errors.Is(err, os.ErrNotExist) {
				return nil // Already downloaded; upload does not need a second copy.
			}
		}
		allocated = min(info.Size(), max(allocatedBytes(info), 0))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	probe := dir
	for {
		if info, err := os.Stat(probe); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("下载缓存目录无效")
			}
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("下载缓存目录不存在")
		}
		probe = parent
	}
	available, err := freeSpace(probe)
	if err != nil {
		return err
	}
	needed := uint64(max(size-allocated, 0)) + (64 << 20)
	if available < needed {
		return &InsufficientSpaceError{Needed: needed, Available: available}
	}
	return nil
}
