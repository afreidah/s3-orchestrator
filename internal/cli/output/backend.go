// -------------------------------------------------------------------------------
// CLI Output - Backend Name Lists
//
// Author: Alex Freidah
//
// Renders a backend's table cells and the instance reporting it the same way in
// admin status and the TUI, so both surfaces describe a backend in the same
// words. The health word itself comes from core.BackendCalls.Health, which the
// web dashboard shares.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// Instance names the instance that answered a status request: its id, the
// build it runs, its mode and how long it has been up. A server too old to
// report its identity renders as "unknown".
func Instance(in adminapi.InstanceStatus) string {
	if in.ID == "" {
		return "unknown"
	}
	return fmt.Sprintf("%s  %s  mode %s  up %s",
		in.ID, in.Version, in.Mode, humanize.Duration(max(time.Since(in.StartedAt), 0)))
}

// BackendCells are the cells admin status and the TUI both render for one
// backend, so the two tables describe a backend in the same words and the
// storage limit reads NoLimit rather than zero for a backend with no quota.
type BackendCells struct {
	Health  string
	Refused string
	Flags   string
	Used    string
	Limit   string
	Use     string
	Objects string
}

// BackendCellsOf renders b's health, refused calls, state flags, storage
// against its quota and object count.
func BackendCellsOf(b *adminapi.BackendStatus) BackendCells {
	storage := StorageCeiling(b)
	return BackendCells{
		Health:  core.CallsNamed(b.RefusedCalls).Health(),
		Refused: NameList(b.RefusedCalls),
		Flags:   NameList(b.Flags),
		Used:    storage.Format(storage.Used),
		Limit:   storage.LimitText(),
		Use:     storage.PercentText(),
		Objects: strconv.FormatInt(b.ObjectCount, 10),
	}
}

// NameList renders a list of names joined by commas, and a dash when it is
// empty, so a table cell is never blank.
func NameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}
