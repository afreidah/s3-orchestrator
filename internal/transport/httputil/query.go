// -------------------------------------------------------------------------------
// HTTP Query Parameter Helpers
//
// Author: Alex Freidah
//
// Shared parsing for the optional count parameters every surface takes: S3's
// paging parameters and the admin and dashboard caps (batch_size, max,
// delay_ms, limit). Absent means the default, and a malformed value is refused
// rather than treated as absent, because the default for a cap like max is the
// whole fleet and a typo must not widen a run.
// -------------------------------------------------------------------------------

package httputil

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// NoCeiling is the ceiling for a count parameter that takes any value up to
// the largest one QueryCount accepts.
const NoCeiling = math.MaxInt32

// QueryCount reads a count parameter the way S3 reads its paging parameters:
// absent or empty means def, a value above ceiling is capped to it, and
// anything that is not an integer between 0 and 2147483647 is refused. The
// error is the message a 400 response carries.
func QueryCount(q url.Values, param string, def, ceiling int) (int, error) {
	s := q.Get(param)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("Argument %s must be an integer between 0 and 2147483647", param) //nolint:staticcheck // ST1005: S3's own message, capitalised as S3 returns it
	}
	return min(int(v), ceiling), nil
}

// RequireQueryCount reads a count parameter with QueryCount. On a malformed
// value it writes a 400 JSON error and returns false so the caller can return
// early.
func RequireQueryCount(w http.ResponseWriter, r *http.Request, param string, def, ceiling int) (int, bool) {
	n, err := QueryCount(r.URL.Query(), param, def, ceiling)
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, err.Error())
		return 0, false
	}
	return n, true
}
