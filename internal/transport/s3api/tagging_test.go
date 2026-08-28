// -------------------------------------------------------------------------------
// Object Tagging Handler Tests
//
// Author: Alex Freidah
//
// Covers the three ?tagging operations at the HTTP boundary: that the
// subresource routes to them rather than falling through to the object itself,
// the Tagging document round-trips, and each store-layer refusal renders as the
// S3 error code the spec names for it rather than a generic 500.
// -------------------------------------------------------------------------------

package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// doTagging issues an authenticated ?tagging request against the test server
// and returns the status and body.
//
// Returns the two values the tests actually assert on rather than the
// *http.Response, so the body is closed here at the one place that opens it
// instead of at eleven call sites.
func doTagging(t *testing.T, url, method, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Proxy-Token", "test-token")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// TestGetObjectTagging_ReturnsSortedDocument verifies the tag set renders as a
// Tagging document ordered by key, so the response is byte-identical run to
// run regardless of what order the store handed the tags back in.
func TestGetObjectTagging_ReturnsSortedDocument(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().GetObjectTags(gomock.Any(), "mybucket/k").
		Return([]core.Tag{{Key: "zeta", Value: "3"}, {Key: "alpha", Value: "1"}}, nil)

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodGet, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}

	var got taggingDocument
	if err := xml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, body)
	}
	if len(got.TagSet.Tags) != 2 {
		t.Fatalf("tag count = %d, want 2: %s", len(got.TagSet.Tags), body)
	}
	if got.TagSet.Tags[0].Key != "alpha" || got.TagSet.Tags[1].Key != "zeta" {
		t.Errorf("tags not sorted by key: %+v", got.TagSet.Tags)
	}
}

// TestGetObjectTagging_UntaggedReturnsEmptySet verifies an object with no tags
// answers 200 with an empty TagSet rather than 404. The object exists; it just
// carries nothing.
func TestGetObjectTagging_UntaggedReturnsEmptySet(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().GetObjectTags(gomock.Any(), "mybucket/k").Return([]core.Tag{}, nil)

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodGet, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, "<Tagging>") {
		t.Errorf("expected a Tagging document, got %s", body)
	}
}

// TestGetObjectTagging_MissingObject verifies a key that holds nothing answers
// NoSuchKey rather than an empty set, so a caller can tell "no tags" apart from
// "no object".
func TestGetObjectTagging_MissingObject(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().GetObjectTags(gomock.Any(), "mybucket/gone").
		Return(nil, core.ErrObjectNotFound)

	status, body := doTagging(t, ts.URL+"/mybucket/gone?tagging", http.MethodGet, "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if !strings.Contains(body, "NoSuchKey") {
		t.Errorf("expected NoSuchKey, got %s", body)
	}
}

// TestPutObjectTagging_ParsesDocument verifies the request body decodes into
// the tag set handed to the store, in the order the document listed it.
func TestPutObjectTagging_ParsesDocument(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().PutObjectTags(gomock.Any(), "mybucket/k",
		[]core.Tag{{Key: "retain", Value: "30d"}, {Key: "team", Value: "infra"}}).Return(nil)

	doc := `<Tagging><TagSet>` +
		`<Tag><Key>retain</Key><Value>30d</Value></Tag>` +
		`<Tag><Key>team</Key><Value>infra</Value></Tag>` +
		`</TagSet></Tagging>`

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut, doc)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
}

// TestPutObjectTagging_EmptySetIsADelete verifies a document with an empty
// TagSet reaches the store as an empty set, which the spec defines as the same
// outcome as DeleteObjectTagging.
func TestPutObjectTagging_EmptySetIsADelete(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().PutObjectTags(gomock.Any(), "mybucket/k", []core.Tag{}).Return(nil)

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut,
		`<Tagging><TagSet></TagSet></Tagging>`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
}

// TestPutObjectTagging_ValidationErrors verifies each store-layer refusal
// renders as the S3 code the spec names, rather than a generic 500.
func TestPutObjectTagging_ValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		storeErr error
		status   int
		code     string
	}{
		{"too many tags", core.ErrTooManyTags, http.StatusBadRequest, "BadRequest"},
		{"empty key", core.ErrEmptyTagKey, http.StatusBadRequest, "InvalidTag"},
		{"key too long", core.ErrTagKeyTooLong, http.StatusBadRequest, "InvalidTag"},
		{"value too long", core.ErrTagValueTooLong, http.StatusBadRequest, "InvalidTag"},
		{"duplicate key", core.ErrDuplicateTagKey, http.StatusBadRequest, "InvalidTag"},
		{"missing object", core.ErrObjectNotFound, http.StatusNotFound, "NoSuchKey"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts, objects, _ := newOpsServer(t)
			objects.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(tc.storeErr)

			status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut,
				`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`)
			if status != tc.status {
				t.Fatalf("status = %d, want %d: %s", status, tc.status, body)
			}
			if !strings.Contains(body, tc.code) {
				t.Errorf("expected %s in body, got %s", tc.code, body)
			}
		})
	}
}

// TestPutObjectTagging_UnmappedErrorFallsBack verifies a failure that is none
// of the named validation refusals still renders as an S3 error rather than
// leaking a Go error string or an empty body.
func TestPutObjectTagging_UnmappedErrorFallsBack(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("database on fire"))

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut,
		`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", status, body)
	}
	if !strings.Contains(body, "InternalError") {
		t.Errorf("expected InternalError, got %s", body)
	}
}

// TestPutObjectTagging_MalformedBody verifies a body that is not a Tagging
// document is refused before anything reaches the store.
func TestPutObjectTagging_MalformedBody(t *testing.T) {
	ts, _, _ := newOpsServer(t)

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut, "not xml at all")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
}

// TestDeleteObjectTagging_RemovesSet verifies the delete answers 204 with no
// body, matching the spec.
func TestDeleteObjectTagging_RemovesSet(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().DeleteObjectTags(gomock.Any(), "mybucket/k").Return(nil)

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodDelete, "")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", status, body)
	}
	if body != "" {
		t.Errorf("expected an empty body, got %q", body)
	}
}

// TestDeleteObjectTagging_MissingObject verifies the delete reports NoSuchKey
// for a key that holds nothing rather than reporting success.
func TestDeleteObjectTagging_MissingObject(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().DeleteObjectTags(gomock.Any(), "mybucket/gone").
		Return(core.ErrObjectNotFound)

	status, body := doTagging(t, ts.URL+"/mybucket/gone?tagging", http.MethodDelete, "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
}

// TestTagging_UnsupportedMethod verifies a verb with no tagging meaning is
// refused rather than reaching the object. Before the subresource was routed,
// a POST here would have fallen through to the plain object dispatch.
func TestTagging_UnsupportedMethod(t *testing.T) {
	ts, _, _ := newOpsServer(t)

	status, _ := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPost, "")
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// TestTagging_DoesNotReachTheObject is the #1234 regression in its tagging
// form: a PUT carrying ?tagging must not be dispatched as PutObject, which
// would replace the object with the tagging document and report success.
func TestTagging_DoesNotReachTheObject(t *testing.T) {
	ts, objects, _ := newOpsServer(t)
	objects.EXPECT().PutObjectTags(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// No PutObject expectation: the mock controller fails the test if the
	// request is dispatched as an object write instead.

	status, body := doTagging(t, ts.URL+"/mybucket/k?tagging", http.MethodPut,
		`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
}
