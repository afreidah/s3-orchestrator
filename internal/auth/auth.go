// -------------------------------------------------------------------------------
// Authentication - AWS Signature Version 4 Verification
//
// Author: Alex Freidah
//
// Implements AWS SigV4 signature verification for S3 client compatibility. Parses
// the Authorization header or presigned URL query parameters, reconstructs the
// canonical request, and verifies the HMAC-SHA256 signature chain. Also supports
// legacy X-Proxy-Token authentication for backward compatibility with simple
// clients.
//
// BucketRegistry maps client credentials to virtual buckets, enabling multi-tenant
// access with per-bucket credential isolation.
// -------------------------------------------------------------------------------

// Package auth provides S3 SigV4 (header and presigned URL) and token-based
// request authentication with multi-bucket credential resolution.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// sigV4MaxSkew is the maximum allowed clock skew for SigV4 request timestamps.
const sigV4MaxSkew = 15 * time.Minute

// presignedMaxExpiry is the maximum allowed expiry for presigned URLs (7 days,
// matching the AWS S3 limit).
const presignedMaxExpiry = 7 * 24 * time.Hour

// -------------------------------------------------------------------------
// BUCKET REGISTRY
// -------------------------------------------------------------------------

// bucketEntry maps an access key to its bucket and signing secret.
type bucketEntry struct {
	BucketName      string
	SecretAccessKey string
}

// BucketRegistry resolves client credentials to virtual bucket names.
type BucketRegistry struct {
	byAccessKey    map[string]bucketEntry // access_key_id -> {bucket, secret}
	byToken        map[string]string      // token -> bucket name
	multipartLimit map[string]int         // bucket name -> max active multipart uploads (0 = unlimited)
}

// NewBucketRegistry builds a credential-to-bucket lookup from the config.
func NewBucketRegistry(buckets []config.BucketConfig) *BucketRegistry {
	br := &BucketRegistry{
		byAccessKey:    make(map[string]bucketEntry),
		byToken:        make(map[string]string),
		multipartLimit: make(map[string]int),
	}

	for _, bkt := range buckets {
		if bkt.MaxMultipartUploads > 0 {
			br.multipartLimit[bkt.Name] = bkt.MaxMultipartUploads
		}
		for _, cred := range bkt.Credentials {
			if cred.AccessKeyID != "" && cred.SecretAccessKey != "" {
				br.byAccessKey[cred.AccessKeyID] = bucketEntry{
					BucketName:      bkt.Name,
					SecretAccessKey: cred.SecretAccessKey,
				}
			}
			if cred.Token != "" {
				br.byToken[cred.Token] = bkt.Name
			}
		}
	}

	return br
}

// MaxMultipartUploads returns the configured limit for active multipart
// uploads on the given bucket. Returns 0 if unlimited.
func (br *BucketRegistry) MaxMultipartUploads(bucket string) int {
	return br.multipartLimit[bucket]
}

// AuthenticateAndResolveBucket authenticates the request and returns the
// authorized bucket name. Returns an error if authentication fails.
func (br *BucketRegistry) AuthenticateAndResolveBucket(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")

	// SigV4 takes precedence. To prevent timing side-channels that could
	// enumerate valid access keys, we always compute the signature — using
	// a dummy secret when the key is unknown — so both paths take the same
	// time.
	if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
		// Parse the header once to extract all three fields.
		parts := authHeader[len("AWS4-HMAC-SHA256 "):]
		credential, signedHeaders, signature := parseSigV4FieldsDirect(parts)
		if credential == "" {
			return "", fmt.Errorf("authentication failed")
		}

		// Extract access key from the credential scope (accessKey/date/region/service/aws4_request).
		accessKey, _, _ := strings.Cut(credential, "/")
		if accessKey == "" {
			return "", fmt.Errorf("authentication failed")
		}

		entry, ok := br.byAccessKey[accessKey]
		secret := "dummy-secret-for-constant-time-auth"
		if ok {
			secret = entry.SecretAccessKey
		}

		if err := verifySigV4Parsed(r, accessKey, secret, credential, signedHeaders, signature, ok); err != nil || !ok {
			return "", fmt.Errorf("authentication failed")
		}

		return entry.BucketName, nil
	}

	// Presigned URL auth — credentials in query string parameters.
	if isPresignedRequest(r) {
		return br.authenticatePresigned(r)
	}

	// Legacy token auth — iterate all tokens to avoid timing side-channels.
	// ConstantTimeCompare requires equal-length inputs, so skip mismatched
	// lengths without leaking which token matched.
	proxyToken := r.Header.Get("X-Proxy-Token")
	if proxyToken != "" {
		var matchedBucket string
		found := 0
		for token, bucket := range br.byToken {
			if len(token) == len(proxyToken) &&
				subtle.ConstantTimeCompare([]byte(proxyToken), []byte(token)) == 1 {
				matchedBucket = bucket
				found = 1
			}
		}
		if found == 1 {
			return matchedBucket, nil
		}
		return "", fmt.Errorf("invalid authentication token")
	}

	return "", fmt.Errorf("missing authentication credentials")
}


// -------------------------------------------------------------------------
// SIGNING KEY CACHE
// -------------------------------------------------------------------------

// signingKeyCache avoids re-deriving the HMAC signing key on every request.
// The key only changes when the dateStamp rolls over (once per day), so
// caching eliminates 4 HMAC-SHA256 operations per request under steady load.
var signingKeyCache sync.Map // accessKeyID → *cachedSigningKey

type cachedSigningKey struct {
	dateStamp string
	region    string
	service   string
	key       []byte
}

// getCachedSigningKey returns the cached signing key for a known access key,
// or derives a fresh one. The knownKey flag controls whether the result is
// cached: unknown access keys (used with a dummy secret for constant-time
// auth) are never cached to prevent an attacker from exhausting memory by
// sending requests with randomized access key IDs.
func getCachedSigningKey(accessKeyID, secret, dateStamp, region, service string, knownKey bool) []byte {
	if knownKey {
		if v, ok := signingKeyCache.Load(accessKeyID); ok {
			c := v.(*cachedSigningKey)
			if c.dateStamp == dateStamp && c.region == region && c.service == service {
				return c.key
			}
		}
	}
	key := deriveSigningKey(secret, dateStamp, region, service)
	if knownKey {
		signingKeyCache.Store(accessKeyID, &cachedSigningKey{
			dateStamp: dateStamp,
			region:    region,
			service:   service,
			key:       key,
		})
	}
	return key
}

// -------------------------------------------------------------------------
// SIGV4 VERIFICATION
// -------------------------------------------------------------------------

// VerifySigV4 checks an AWS Signature Version 4 Authorization header against
// the provided credentials. The caller is responsible for resolving the correct
// credentials via BucketRegistry. Returns nil if the signature is valid.
func VerifySigV4(r *http.Request, accessKeyID, secretAccessKey string) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing Authorization header")
	}

	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
		return fmt.Errorf("unsupported auth scheme")
	}

	parts := authHeader[len("AWS4-HMAC-SHA256 "):]
	credential, signedHeadersStr, signature := parseSigV4FieldsDirect(parts)
	if credential == "" || signedHeadersStr == "" || signature == "" {
		return fmt.Errorf("malformed Authorization header")
	}

	return verifySigV4Parsed(r, accessKeyID, secretAccessKey, credential, signedHeadersStr, signature, true)
}

// verifySigV4Parsed verifies the signature using pre-parsed Authorization
// header fields, avoiding a redundant parse when called from
// AuthenticateAndResolveBucket.
func verifySigV4Parsed(r *http.Request, accessKeyID, secretAccessKey, credential, signedHeadersStr, signature string, knownKey bool) error {
	// Parse credential: accessKeyID/date/region/service/aws4_request
	credParts := strings.SplitN(credential, "/", 5)
	if len(credParts) != 5 {
		return fmt.Errorf("malformed credential scope")
	}

	dateStamp := credParts[1]
	region := credParts[2]
	service := credParts[3]

	// Build canonical request
	signedHeaders := strings.Split(signedHeadersStr, ";")

	// The host header must always be signed per the SigV4 spec to prevent
	// replay attacks against different endpoints.
	if !slices.Contains(signedHeaders, "host") {
		return fmt.Errorf("host header must be signed")
	}

	canonicalRequest := buildCanonicalRequest(r, signedHeaders)

	// Validate request timestamp to prevent replay attacks
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return fmt.Errorf("missing X-Amz-Date header")
	}

	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("malformed X-Amz-Date: %w", err)
	}
	if skew := time.Since(reqTime).Abs(); skew > sigV4MaxSkew {
		return fmt.Errorf("request timestamp too skewed (%s)", skew.Truncate(time.Second))
	}

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))

	// Derive signing key (cached for known keys only)
	signingKey := getCachedSigningKey(accessKeyID, secretAccessKey, dateStamp, region, service, knownKey)

	// Calculate expected signature
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

// -------------------------------------------------------------------------
// PRESIGNED URL VERIFICATION
// -------------------------------------------------------------------------

// isPresignedRequest returns true if the request carries SigV4 credentials
// in query string parameters (presigned URL).
func isPresignedRequest(r *http.Request) bool {
	return r.URL.Query().Get("X-Amz-Algorithm") != ""
}

// authenticatePresigned extracts SigV4 credentials from query string
// parameters and verifies the presigned URL signature.
func (br *BucketRegistry) authenticatePresigned(r *http.Request) (string, error) {
	q := r.URL.Query()
	credential := q.Get("X-Amz-Credential")
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	signature := q.Get("X-Amz-Signature")
	amzDate := q.Get("X-Amz-Date")
	expires := q.Get("X-Amz-Expires")

	if credential == "" || signedHeaders == "" || signature == "" || amzDate == "" || expires == "" {
		return "", fmt.Errorf("authentication failed")
	}

	accessKey, _, _ := strings.Cut(credential, "/")
	if accessKey == "" {
		return "", fmt.Errorf("authentication failed")
	}

	entry, ok := br.byAccessKey[accessKey]
	secret := "dummy-secret-for-constant-time-auth"
	if ok {
		secret = entry.SecretAccessKey
	}

	if err := verifyPresignedSigV4(r, accessKey, secret, credential, signedHeaders, signature, amzDate, expires, ok); err != nil || !ok {
		return "", fmt.Errorf("authentication failed")
	}

	return entry.BucketName, nil
}

// verifyPresignedSigV4 verifies a presigned URL signature. Unlike header-based
// SigV4, the date and expiry come from query parameters, the X-Amz-Signature
// parameter is excluded from the canonical query string, and the payload hash
// is always UNSIGNED-PAYLOAD.
func verifyPresignedSigV4(r *http.Request, accessKeyID, secretAccessKey, credential, signedHeadersStr, signature, amzDate, expiresStr string, knownKey bool) error {
	credParts := strings.SplitN(credential, "/", 5)
	if len(credParts) != 5 {
		return fmt.Errorf("malformed credential scope")
	}

	dateStamp := credParts[1]
	region := credParts[2]
	service := credParts[3]

	signedHeaders := strings.Split(signedHeadersStr, ";")

	if !slices.Contains(signedHeaders, "host") {
		return fmt.Errorf("host header must be signed")
	}

	canonicalRequest := buildPresignedCanonicalRequest(r, signedHeaders)

	// Validate presigned URL timestamp and expiry
	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("malformed X-Amz-Date: %w", err)
	}

	expirySecs, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || expirySecs <= 0 {
		return fmt.Errorf("invalid X-Amz-Expires value")
	}
	expiryDuration := time.Duration(expirySecs) * time.Second
	if expiryDuration > presignedMaxExpiry {
		return fmt.Errorf("X-Amz-Expires exceeds maximum (%s)", presignedMaxExpiry)
	}
	if time.Now().After(reqTime.Add(expiryDuration)) {
		return fmt.Errorf("presigned URL has expired")
	}

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))

	signingKey := getCachedSigningKey(accessKeyID, secretAccessKey, dateStamp, region, service, knownKey)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

// buildPresignedCanonicalRequest constructs the canonical request for presigned
// URL verification. Differs from the header-based variant in two ways:
// X-Amz-Signature is excluded from the canonical query string, and the payload
// hash is always UNSIGNED-PAYLOAD.
func buildPresignedCanonicalRequest(r *http.Request, signedHeaders []string) string {
	var b strings.Builder
	b.Grow(512) // presigned URLs have more query params than header-based

	// Method
	b.WriteString(r.Method)
	b.WriteByte('\n')

	// Canonical URI
	encodePath(&b, r.URL.Path)
	b.WriteByte('\n')

	// Canonical query string (excluding X-Amz-Signature)
	qv := make(url.Values)
	for k, vs := range r.URL.Query() {
		if k != "X-Amz-Signature" {
			qv[k] = vs
		}
	}
	buildCanonicalQueryString(&b, qv)
	b.WriteByte('\n')

	// Canonical headers
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		val := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && val == "" {
			val = r.Host
		}
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	// Signed headers
	for i, h := range signedHeaders {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(h)
	}
	b.WriteByte('\n')

	// Payload hash — always UNSIGNED-PAYLOAD for presigned URLs
	b.WriteString("UNSIGNED-PAYLOAD")

	return b.String()
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// parseSigV4Fields extracts key=value pairs from the SigV4 auth header.
// Retained for use by extractAccessKey which only needs the Credential field.
func parseSigV4Fields(s string) map[string]string {
	fields := make(map[string]string)
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		idx := strings.IndexByte(part, '=')
		if idx > 0 {
			fields[part[:idx]] = part[idx+1:]
		}
	}
	return fields
}

// parseSigV4FieldsDirect extracts Credential, SignedHeaders, and Signature
// from the SigV4 auth header without allocating a map.
func parseSigV4FieldsDirect(s string) (credential, signedHeaders, signature string) {
	for s != "" {
		part := s
		if i := strings.IndexByte(s, ','); i >= 0 {
			part = s[:i]
			s = s[i+1:]
		} else {
			s = ""
		}
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, '='); i > 0 {
			switch part[:i] {
			case "Credential":
				credential = part[i+1:]
			case "SignedHeaders":
				signedHeaders = part[i+1:]
			case "Signature":
				signature = part[i+1:]
			}
		}
	}
	return
}

// buildCanonicalRequest constructs the canonical request string per SigV4 spec.
func buildCanonicalRequest(r *http.Request, signedHeaders []string) string {
	var b strings.Builder
	b.Grow(256) // typical canonical request fits in 256 bytes

	// Method
	b.WriteString(r.Method)
	b.WriteByte('\n')

	// Canonical URI
	encodePath(&b, r.URL.Path)
	b.WriteByte('\n')

	// Canonical query string
	buildCanonicalQueryString(&b, r.URL.Query())
	b.WriteByte('\n')

	// Canonical headers
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		val := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && val == "" {
			val = r.Host
		}
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	// Signed headers
	for i, h := range signedHeaders {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(h)
	}
	b.WriteByte('\n')

	// Payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	b.WriteString(payloadHash)

	return b.String()
}

// buildCanonicalQueryString writes sorted, URI-encoded query parameters per
// the SigV4 spec (RFC 3986 encoding where spaces become %20, not +).
func buildCanonicalQueryString(b *strings.Builder, values url.Values) {
	if len(values) == 0 {
		return
	}

	params := make([]string, 0, len(values))
	for k, vs := range values {
		for _, v := range vs {
			params = append(params, sigV4Encode(k)+"="+sigV4Encode(v))
		}
	}
	sort.Strings(params)
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p)
	}
}

// sigV4Encode performs URI encoding per the SigV4 spec: RFC 3986 unreserved
// characters (A-Z, a-z, 0-9, '-', '.', '_', '~') are not encoded, everything
// else is percent-encoded. Unlike url.QueryEscape, spaces become %20 not +.
func sigV4Encode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// encodePath URI-encodes each path segment per the SigV4 spec directly into
// the builder. Slashes are preserved as literal separators.
func encodePath(b *strings.Builder, rawPath string) {
	if rawPath == "" || rawPath == "/" {
		b.WriteByte('/')
		return
	}
	for i, seg := range strings.Split(rawPath, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(sigV4Encode(seg))
	}
}

// deriveSigningKey computes the SigV4 signing key from the secret.
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// hashSHA256 computes a hex-encoded SHA256 hash.
func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
