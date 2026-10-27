// -------------------------------------------------------------------------------
// Backend Features
//
// Author: Alex Freidah
//
// The optional S3 features a backend has been found not to support, as a bit
// per feature. Not every S3-compatible provider implements everything the
// orchestrator can use, and each feature has a fallback that works without it.
// A backend learns a feature is missing from the first refusal and takes the
// fallback from then on, without asking again.
//
// The set lives in memory per instance for the life of the process and is
// never stored, so bits may be renumbered.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"

	"github.com/afreidah/s3-orchestrator/internal/util/bitset"
)

// BackendFeatures is a set of optional backend features. A backend holds the
// ones it has found unsupported.
type BackendFeatures uint64

// The optional features. Range is a ranged GET answered with only the range.
// BatchDelete is multi-object DeleteObjects. NativeCopy is a server-side
// CopyObject within the backend's bucket.
const (
	FeatureRange BackendFeatures = 1 << iota
	FeatureBatchDelete
	FeatureNativeCopy
)

// backendFeatureNames pairs each feature with its name, in the order a
// rendered set lists them.
var backendFeatureNames = bitset.Vocabulary[BackendFeatures]{
	{Bit: FeatureRange, Name: "range"},
	{Bit: FeatureBatchDelete, Name: "batch-delete"},
	{Bit: FeatureNativeCopy, Name: "native-copy"},
}

// Has reports whether the set carries every feature in want.
func (f BackendFeatures) Has(want BackendFeatures) bool {
	return bitset.Has(f, want)
}

// Names lists the features in the set by name.
func (f BackendFeatures) Names() []string {
	return backendFeatureNames.Names(f)
}

// String renders the set as its feature names joined by commas.
func (f BackendFeatures) String() string {
	return backendFeatureNames.Join(f)
}

// MarshalJSON renders the set as its feature names, so an API carrying it
// reads ["range"] rather than a bit number.
func (f BackendFeatures) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.Names())
}
