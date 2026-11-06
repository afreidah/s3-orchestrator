// -------------------------------------------------------------------------------
// Declared Digest Tests
//
// Author: Alex Freidah
//
// A client states what its body should hash to through Content-MD5, a checksum
// header, or a signed payload hash. These tests send a body that disagrees with
// each and check the request is refused with the code AWS uses, and that nothing
// is stored.
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: Content-MD5 is the S3 wire format
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/testutil/sigv4test"
)

const (
	digestBody     = "the bytes the client sent"
	digestOther    = "some other bytes"
	digestObject   = "/mybucket/digest-key"
	headerMD5      = "Content-MD5"
	codeBadDigest  = "BadDigest"
	headerChecksum = "X-Amz-Checksum-Sha256"
)

// md5Of returns data's MD5 as a Content-MD5 header carries it.
func md5Of(data string) string {
	sum := md5.Sum([]byte(data)) //nolint:gosec // G401: Content-MD5 is the S3 wire format
	return base64.StdEncoding.EncodeToString(sum[:])
}

// sha256Of returns data's SHA-256 as a checksum header carries it.
func sha256Of(data string) string {
	sum := sha256.Sum256([]byte(data))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// sha256Hex returns data's SHA-256 as a signed payload hash carries it.
func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// sendDeclared sends digestBody to path with header set and, when payloadHash is
// not empty, signed as that hash. It returns the status and response body.
func sendDeclared(t *testing.T, ts *httptest.Server, path, header, value, payloadHash string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+path, strings.NewReader(digestBody))
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	if payloadHash != "" {
		sigv4test.SignPayload(t, req, testAccessKeyID, testSecretKey, payloadHash)
	} else {
		signRequest(t, req)
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	if _, err := io.Copy(&body, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body.String()
}

// TestPut_DeclaredDigests verifies a body matching its declared digest is
// stored, and one that does not is refused and leaves nothing behind.
func TestPut_DeclaredDigests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, header, value, payloadHash string
		wantCode                         string
	}{
		{name: "content md5 matches", header: headerMD5, value: md5Of(digestBody)},
		{name: "content md5 differs", header: headerMD5, value: md5Of(digestOther), wantCode: codeBadDigest},
		{name: "content md5 malformed", header: headerMD5, value: "not base64!", wantCode: "InvalidDigest"},
		{name: "content md5 wrong length", header: headerMD5, value: "AAAA", wantCode: "InvalidDigest"},
		{name: "checksum header matches", header: headerChecksum, value: sha256Of(digestBody)},
		{name: "checksum header differs", header: headerChecksum, value: sha256Of(digestOther), wantCode: codeBadDigest},
		{name: "checksum header malformed", header: headerChecksum, value: "AAAA", wantCode: "InvalidRequest"},
		{name: "payload hash matches", payloadHash: sha256Hex(digestBody)},
		{name: "payload hash differs", payloadHash: sha256Hex(digestOther), wantCode: "XAmzContentSHA256Mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, _, backend := newTestServer(t)
			status, body := sendDeclared(t, ts, digestObject, tc.header, tc.value, tc.payloadHash)
			assertDigestOutcome(t, status, body, tc.wantCode)
			if tc.wantCode != "" && backend.HasCopyOf("mybucket/digest-key") {
				t.Error("a refused body was stored")
			}
		})
	}
}

// assertDigestOutcome checks the response against wantCode, where an empty code
// means the request should have succeeded.
func assertDigestOutcome(t *testing.T, status int, body, wantCode string) {
	t.Helper()
	if wantCode == "" {
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200. body: %s", status, body)
		}
		return
	}
	if status != http.StatusBadRequest || !strings.Contains(body, wantCode) {
		t.Errorf("status = %d, body %s; want 400 %s", status, body, wantCode)
	}
}

// TestUploadPart_DigestMismatch verifies a part is held to its digest as an
// object is, though its bytes stream to the backend rather than being buffered.
func TestUploadPart_DigestMismatch(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).
			Return(&core.MultipartUpload{
				UploadID: "upload-1", ObjectKey: "mybucket/digest-key", BackendName: "b1",
				ContentType: "application/octet-stream",
			}, nil).AnyTimes()
		m.EXPECT().RecordPart(gomock.Any(), gomock.Any()).Times(0)
	})
	status, body := sendDeclared(t, ts, digestObject+"?uploadId=upload-1&partNumber=1", headerMD5, md5Of(digestOther), "")
	if status != http.StatusBadRequest || !strings.Contains(body, codeBadDigest) {
		t.Errorf("status = %d, body %s; want 400 BadDigest", status, body)
	}
}

// TestDeleteObjects_DigestMismatch verifies an XML body is held to its digest too:
// the decoder reads to the end of the document before anything is deleted.
func TestDeleteObjects_DigestMismatch(t *testing.T) {
	t.Parallel()
	ts, _, _ := newTestServer(t, func(m *storetest.MockMetadataStore) {
		m.EXPECT().DeleteObjectsBatch(gomock.Any(), gomock.Any()).Times(0)
	})
	body := `<Delete><Object><Key>k</Key></Object></Delete>`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mybucket?delete", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(headerMD5, md5Of(digestOther))
	signRequest(t, req)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(got), codeBadDigest) {
		t.Errorf("status = %d, body %s; want 400 BadDigest", resp.StatusCode, got)
	}
}

// TestRequestMetrics_ClampUnknownMethods verifies a method name a client invents
// never becomes a metric label. The inflight gauge runs before authentication,
// so an anonymous client could otherwise create series without limit.
func TestRequestMetrics_ClampUnknownMethods(t *testing.T) {
	t.Parallel()
	const invented = "INVENTEDMETHOD"
	ts, _, _ := newTestServer(t)
	req, err := http.NewRequestWithContext(context.Background(), invented, ts.URL+digestObject, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if methodSeries(t, invented) != 0 {
		t.Errorf("metrics carry a series labeled %s", invented)
	}
}
