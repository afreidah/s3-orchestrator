// -------------------------------------------------------------------------------
// Runtime - Go Memory Limit From the Container
//
// Author: Alex Freidah
//
// Sets the Go soft memory limit from the cgroup v2 memory limit the process runs
// under, so the garbage collector works harder before the kernel's OOM killer
// acts. Go sets GOMAXPROCS from the cgroup CPU limit itself but has no memory
// equivalent. An explicit GOMEMLIMIT always wins.
// -------------------------------------------------------------------------------

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// procSelfCgroup names the cgroup the process runs in, and cgroupRoot is where
// the cgroup v2 hierarchy is mounted. In a container the process's own cgroup
// is the namespace root; under systemd it is the unit's group below it.
const (
	procSelfCgroup = "/proc/self/cgroup"
	cgroupRoot     = "/sys/fs/cgroup"
)

// memoryLimitShare is the part of the cgroup limit given to the Go runtime. The
// rest covers what the runtime does not count: thread stacks outside the heap,
// the binary's own pages, and memory the kernel charges to the cgroup.
const memoryLimitShare = 0.9

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// cgroupMemoryMaxPath returns the memory.max file of the cgroup v2 group named
// in procCgroup, under root, or "" when the file names no v2 group.
func cgroupMemoryMaxPath(procCgroup, root string) string {
	data, err := os.ReadFile(procCgroup)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if group, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(root, group, "memory.max")
		}
	}
	return ""
}

// containerMemoryLimit returns the soft memory limit to apply for the cgroup
// limit in path, or 0 when GOMEMLIMIT is set, the file cannot be read, or the
// cgroup has no limit.
func containerMemoryLimit(path string) int64 {
	if os.Getenv("GOMEMLIMIT") != "" || path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || limit <= 0 {
		return 0
	}
	return int64(float64(limit) * memoryLimitShare)
}
