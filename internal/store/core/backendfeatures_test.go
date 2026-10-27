// -------------------------------------------------------------------------------
// Backend Features - Tests
//
// Author: Alex Freidah
//
// The feature vocabulary and how a set renders: by name, joined, and as JSON.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestBackendFeatures_BitsAreDistinct pins that every feature occupies its own
// bit.
func TestBackendFeatures_BitsAreDistinct(t *testing.T) {
	t.Parallel()
	if err := backendFeatureNames.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestBackendFeatures_Render covers Has, the names, the joined form and the
// JSON form, which an empty set renders as an empty list.
func TestBackendFeatures_Render(t *testing.T) {
	t.Parallel()
	both := FeatureRange | FeatureNativeCopy
	if !both.Has(FeatureNativeCopy) || both.Has(FeatureBatchDelete) {
		t.Errorf("Has on %s is wrong", both)
	}
	if got := both.Names(); !slices.Equal(got, []string{"range", "native-copy"}) {
		t.Errorf("Names = %v", got)
	}
	if got := both.String(); got != "range,native-copy" {
		t.Errorf("String = %q", got)
	}
	for features, want := range map[BackendFeatures]string{
		0:                  `[]`,
		FeatureBatchDelete: `["batch-delete"]`,
	} {
		got, err := json.Marshal(features)
		if err != nil || string(got) != want {
			t.Errorf("json.Marshal(%d) = %s, %v; want %s", uint64(features), got, err, want)
		}
	}
}
