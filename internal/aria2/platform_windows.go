package aria2

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
func processAlive(pid int) bool {
	h, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if e != nil {
		return e != windows.ERROR_INVALID_PARAMETER
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == 259
}
func freeSpace(path string) (uint64, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	var free uint64
	e = windows.GetDiskFreeSpaceEx(p, &free, nil, nil)
	return free, e
}

func allocatedBytes(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Win32FileAttributeData); ok &&
		stat.FileAttributes&(windows.FILE_ATTRIBUTE_SPARSE_FILE|windows.FILE_ATTRIBUTE_COMPRESSED) == 0 {
		return info.Size()
	}
	return 0 // Conservatively reserve the whole file for sparse/compressed data.
}
