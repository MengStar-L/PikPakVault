package aria2

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
func processAlive(pid int) bool {
	if b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); e == nil {
		end := strings.LastIndex(string(b), ")")
		if end >= 0 && len(b) > end+2 && b[end+2] == 'Z' {
			return false
		}
	}
	return unix.Kill(pid, 0) != unix.ESRCH
}
func freeSpace(path string) (uint64, error) {
	var s unix.Statfs_t
	e := unix.Statfs(path, &s)
	return s.Bavail * uint64(s.Bsize), e
}

func allocatedBytes(info os.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Blocks * 512
	}
	return 0 // Unknown allocation must never count sparse holes as saved bytes.
}
