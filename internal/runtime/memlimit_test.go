// -------------------------------------------------------------------------------
// Runtime - Go Memory Limit From the Container - Tests
//
// Author: Alex Freidah
//
// Covers finding the process's cgroup v2 group and reading its memory limit: a
// limit yields 90% of it, and an unlimited cgroup, an unreadable file or an
// explicit GOMEMLIMIT yield nothing.
// -------------------------------------------------------------------------------

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMemoryMax writes content as a memory.max file and returns its path.
func writeMemoryMax(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestContainerMemoryLimit_FromCgroup sets 90% of a cgroup limit and nothing
// when the cgroup is unlimited, unreadable or malformed.
func TestContainerMemoryLimit_FromCgroup(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	tests := []struct {
		name string
		path string
		want int64
	}{
		{"limited", writeMemoryMax(t, "1073741824\n"), 966367641},
		{"unlimited", writeMemoryMax(t, "max\n"), 0},
		{"malformed", writeMemoryMax(t, "lots\n"), 0},
		{"missing", filepath.Join(t.TempDir(), "absent"), 0},
		{"no group", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerMemoryLimit(tt.path); got != tt.want {
				t.Errorf("containerMemoryLimit = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCgroupMemoryMaxPath_FromProcSelfCgroup finds the process's own group:
// the namespace root in a container, the unit's group under systemd, and
// nothing on a cgroup v1 host.
func TestCgroupMemoryMaxPath_FromProcSelfCgroup(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"container", "0::/\n", "/sys/fs/cgroup/memory.max"},
		{"systemd unit", "0::/system.slice/s3-orchestrator.service\n", "/sys/fs/cgroup/system.slice/s3-orchestrator.service/memory.max"},
		{"cgroup v1", "12:memory:/user.slice\n1:name=systemd:/user.slice\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cgroup")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := cgroupMemoryMaxPath(path, "/sys/fs/cgroup"); got != tt.want {
				t.Errorf("cgroupMemoryMaxPath = %q, want %q", got, tt.want)
			}
		})
	}
	if got := cgroupMemoryMaxPath(filepath.Join(t.TempDir(), "absent"), "/sys/fs/cgroup"); got != "" {
		t.Errorf("cgroupMemoryMaxPath of a missing file = %q, want empty", got)
	}
}

// TestContainerMemoryLimit_ExplicitSettingWins leaves the limit to the runtime
// when GOMEMLIMIT is set, whatever the cgroup allows.
func TestContainerMemoryLimit_ExplicitSettingWins(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "512MiB")
	if got := containerMemoryLimit(writeMemoryMax(t, "1073741824\n")); got != 0 {
		t.Errorf("containerMemoryLimit = %d, want 0", got)
	}
}
