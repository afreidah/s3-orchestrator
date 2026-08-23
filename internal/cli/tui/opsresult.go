// -------------------------------------------------------------------------------
// TUI - One-Shot Action Results
//
// Author: Alex Freidah
//
// The admin actions that finish in a single round trip answer with one JSON
// summary. Each is decoded into the adminapi type its endpoint declares and
// reported in the operation's own words, so the ops pane shows "moved 12
// objects" rather than the response fields spelled back as key=value pairs.
// The long-running actions stream their own progress and do not come through
// here.
// -------------------------------------------------------------------------------

package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// The two status values a worker reports when the pass did not run.
const (
	statusSkipped  = "skipped"
	statusDisabled = "disabled"
)

// opsResult is a decoded one-shot response that can account for itself.
type opsResult interface {
	// skipReason is why the pass did not run, or "" when it did.
	skipReason() string
	// describe states what the pass changed.
	describe() string
}

// oneShotDecoder turns a non-streaming action's response body into the single
// terminal event the ops pane renders.
type oneShotDecoder func(io.Reader) (adminstream.Event, error)

// decodeOneShot decodes a response into T and renders it as one result event,
// so a one-shot action displays exactly as a streamed one's final event does.
func decodeOneShot[T opsResult](r io.Reader) (adminstream.Event, error) {
	var res T
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		return adminstream.Event{}, err
	}
	if reason := res.skipReason(); reason != "" {
		return adminstream.Event{
			Kind:    adminstream.KindResult,
			Outcome: adminstream.OutcomeSkipped,
			Message: reason,
		}, nil
	}
	return adminstream.Event{
		Kind:    adminstream.KindResult,
		Outcome: adminstream.OutcomeOK,
		Message: res.describe(),
	}, nil
}

// usageReconcileResult reports the corrections applied to the per-backend usage
// counters.
type usageReconcileResult struct {
	adminapi.UsageReconcileResponse
}

func (r usageReconcileResult) skipReason() string { return skippedBecause(r.Status) }

func (r usageReconcileResult) describe() string {
	if len(r.Adjustments) == 0 {
		return "counters already accurate"
	}
	names := slices.Sorted(maps.Keys(r.Adjustments))

	deltas := make([]string, 0, len(names))
	for _, name := range names {
		deltas = append(deltas, name+" "+signedSize(r.Adjustments[name]))
	}
	return fmt.Sprintf("corrected %s: %s",
		countOf(len(names), "backend", "backends"), strings.Join(deltas, ", "))
}

// cacheFlushResult reports how much of the object data cache a flush dropped.
type cacheFlushResult struct {
	adminapi.CacheInvalidateResponse
}

func (r cacheFlushResult) skipReason() string { return skippedBecause(r.Status) }

func (r cacheFlushResult) describe() string {
	if r.EntriesDropped == 0 {
		return "cache was already empty"
	}
	return "dropped " + countOf(r.EntriesDropped, "cache entry", "cache entries")
}

// cacheInvalidateKeyResult reports one key dropped from the cache. The cache
// treats an unknown key as a no-op, so the answer names the key rather than
// claiming a count it cannot know.
type cacheInvalidateKeyResult struct {
	adminapi.CacheInvalidateKeyResponse
}

func (r cacheInvalidateKeyResult) skipReason() string { return skippedBecause(r.Status) }

func (r cacheInvalidateKeyResult) describe() string { return "invalidated " + r.Key }

// usageFlushResult reports that the buffered usage counters reached the
// database. The endpoint answers with a status alone, so there is no count to
// report back.
type usageFlushResult struct {
	adminapi.UsageFlushResponse
}

func (r usageFlushResult) skipReason() string { return skippedBecause(r.Status) }

func (r usageFlushResult) describe() string { return "counters flushed to the database" }

// encryptExistingResult reports a pass that rewrote plaintext copies as
// ciphertext.
type encryptExistingResult struct {
	adminapi.EncryptExistingResponse
}

func (r encryptExistingResult) skipReason() string { return skippedBecause(r.Status) }

func (r encryptExistingResult) describe() string {
	return rewriteSummary("encrypted", r.Encrypted, r.Failed, r.Total)
}

// decryptExistingResult reports a pass that rewrote encrypted copies as
// plaintext.
type decryptExistingResult struct {
	adminapi.DecryptExistingResponse
}

func (r decryptExistingResult) skipReason() string { return skippedBecause(r.Status) }

func (r decryptExistingResult) describe() string {
	return rewriteSummary("decrypted", r.Decrypted, r.Failed, r.Total)
}

// The compression passes have no one-shot result type here: both stream their
// progress, so the TUI renders the step events and the terminal summary the
// server sends rather than decoding a final JSON body.

// rotateKeyResult reports a pass that re-wrapped object keys under the current
// primary key.
type rotateKeyResult struct {
	adminapi.RotateEncryptionKeyResponse
}

func (r rotateKeyResult) skipReason() string { return skippedBecause(r.Status) }

func (r rotateKeyResult) describe() string {
	return rewriteSummary("rotated", r.Rotated, r.Failed, r.Total)
}

// rewriteSummary words a fleet-wide rewrite pass. A pass that left objects
// behind says so, since "encrypted 900" reads as done when 100 failed.
func rewriteSummary(verb string, succeeded, failed, total int) string {
	if total == 0 {
		return "nothing to " + strings.TrimSuffix(verb, "ed")
	}
	summary := verb + " " + countOf(succeeded, "object", "objects")
	if failed > 0 {
		summary += fmt.Sprintf(", %s failed", grouped(failed))
	}
	return summary
}

// skippedBecause reports why a pass did not run, or "" when it did. Neither
// one-shot response carries a reason field, so the status is the whole of the
// explanation the endpoint offers.
func skippedBecause(status string) string {
	if status == statusSkipped || status == statusDisabled {
		return status
	}
	return ""
}

// countOf renders a count with its noun, grouped for readability.
func countOf(n int, singular, plural string) string {
	noun := plural
	if n == 1 {
		noun = singular
	}
	return grouped(n) + " " + noun
}

// grouped renders an integer with thousands separators so large counts read at
// a glance.
func grouped(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

// signedSize renders a byte delta with an explicit sign, so a correction reads
// as a direction and not just a magnitude.
func signedSize(delta int64) string {
	if delta < 0 {
		return humanize.Bytes(delta) // the formatter carries the minus sign
	}
	return "+" + humanize.Bytes(delta)
}
