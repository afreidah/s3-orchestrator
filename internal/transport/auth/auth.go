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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// sigV4MaxSkew is the maximum allowed clock skew for SigV4 request timestamps.
const sigV4MaxSkew = 15 * time.Minute

// presignedMaxExpiry is the maximum allowed expiry for presigned URLs (7 days,
// matching the AWS S3 limit).
const presignedMaxExpiry = 7 * 24 * time.Hour

// errAuthFailed and related constants used by this package.
const (
	errAuthFailed = "authentication failed"
	sigV4Prefix   = "AWS4-HMAC-SHA256 "
)

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

// AuthenticateAndResolveBucket authenticates the request and returns
// the authorized bucket name plus, when the SigV4 seed signature
// declares a streaming payload, the StreamingMaterial that the transport
// layer needs to verify and decode the chunk chain. The streaming return
// is nil for non-streaming requests, presigned URLs, and proxy-token
// authentication.
func (br *BucketRegistry) AuthenticateAndResolveBucket(r *http.Request) (string, *StreamingMaterial, error) {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, sigV4Prefix) {
		return br.authenticateSigV4(r, authHeader)
	}
	if isPresignedRequest(r) {
		bucket, err := br.authenticatePresigned(r)
		return bucket, nil, err
	}
	if proxyToken := r.Header.Get("X-Proxy-Token"); proxyToken != "" {
		bucket, err := br.authenticateProxyToken(proxyToken)
		return bucket, nil, err
	}
	return "", nil, fmt.Errorf("missing authentication credentials")
}

// authenticateSigV4 verifies a SigV4 Authorization header against the
// registry. To prevent timing side-channels that could enumerate valid
// access keys, the signature is always computed - with a dummy secret
// when the key is unknown - so both paths take the same time.
func (br *BucketRegistry) authenticateSigV4(r *http.Request, authHeader string) (string, *StreamingMaterial, error) {
	parts := authHeader[len(sigV4Prefix):]
	credential, signedHeaders, signature := parseSigV4FieldsDirect(parts)
	if credential == "" {
		return "", nil, errors.New(errAuthFailed)
	}
	accessKey, _, _ := strings.Cut(credential, "/")
	if accessKey == "" {
		return "", nil, errors.New(errAuthFailed)
	}

	entry, ok := br.byAccessKey[accessKey]
	secret := "dummy-secret-for-constant-time-auth"
	if ok {
		secret = entry.SecretAccessKey
	}
	mat, err := verifySigV4Parsed(r,
		keyMaterial{AccessKeyID: accessKey, SecretAccessKey: secret, Known: ok},
		credential, signedHeaders, signature)
	if err != nil || !ok {
		return "", nil, errors.New(errAuthFailed)
	}
	return entry.BucketName, mat, nil
}

// authenticateProxyToken matches an X-Proxy-Token header against the
// registry in constant time. Iterates every entry to avoid leaking which
// token matched; ConstantTimeCompare requires equal-length inputs, so
// mismatched lengths short-circuit without revealing the match position.
func (br *BucketRegistry) authenticateProxyToken(proxyToken string) (string, error) {
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

// -------------------------------------------------------------------------
// SIGV4 VERIFICATION
// -------------------------------------------------------------------------

// Note on signing-key derivation timing.
//
// SigV4 verification derives a per-request signing key via four chained
// HMAC-SHA256 operations (date, region, service, "aws4_request"). The
// straightforward optimization is to memoize the result per access-key
// for the duration of a date+region+service window  -  the inputs only
// change when the dateStamp rolls over (once per day)  -  but that creates
// a request-latency side channel: cache hits skip the HMACs while
// cache-miss / unknown-key paths run them every time. An attacker who
// times responses can then distinguish "is this access key registered?"
// without ever producing a valid signature.
//
// We therefore derive on every request, paying ~4 HMAC-SHA256 ops
// (~few microseconds, dwarfed by network latency) to keep both paths
// timing-equivalent. The previous signingKeyCache var was removed for
// this reason; do not reintroduce it without a constant-time strategy
// that doesn't leak known-vs-unknown.

// keyMaterial bundles the access-key triple SigV4 verification needs
// to derive the signing key. The Known flag distinguishes a real
// caller-supplied key from the dummy fallback used to keep authentication
// constant-time when the access key is not registered.
type keyMaterial struct {
	AccessKeyID     string
	SecretAccessKey string
	Known           bool
}

// VerifySigV4 checks an AWS Signature Version 4 Authorization header against
// the provided credentials. The caller is responsible for resolving the correct
// credentials via BucketRegistry. Returns nil if the signature is valid.
func VerifySigV4(r *http.Request, accessKeyID, secretAccessKey string) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing Authorization header")
	}

	if !strings.HasPrefix(authHeader, sigV4Prefix) {
		return fmt.Errorf("unsupported auth scheme")
	}

	parts := authHeader[len(sigV4Prefix):]
	credential, signedHeadersStr, signature := parseSigV4FieldsDirect(parts)
	if credential == "" || signedHeadersStr == "" || signature == "" {
		return fmt.Errorf("malformed Authorization header")
	}

	_, err := verifySigV4Parsed(r,
		keyMaterial{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey, Known: true},
		credential, signedHeadersStr, signature)
	return err
}

// verifySigV4Parsed verifies the signature using pre-parsed Authorization
// header fields and, when the request declares a streaming payload via
// the X-Amz-Content-Sha256 header, returns the StreamingMaterial the
// transport layer needs to verify the chunk chain. Avoids a redundant
// parse when called from AuthenticateAndResolveBucket.
func verifySigV4Parsed(r *http.Request, key keyMaterial, credential, signedHeadersStr, signature string) (*StreamingMaterial, error) {
	dateStamp, region, service, signedHeaders, err := parseSigV4Credential(credential, signedHeadersStr)
	if err != nil {
		return nil, err
	}

	// Validate request timestamp to prevent replay attacks. The header
	// variant additionally enforces a skew window  -  presigned URLs use
	// X-Amz-Expires and have their own freshness check.
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return nil, fmt.Errorf("missing X-Amz-Date header")
	}
	reqTime, err := parseSigV4Time(amzDate, dateStamp)
	if err != nil {
		return nil, err
	}
	if skew := time.Since(reqTime).Abs(); skew > sigV4MaxSkew {
		return nil, fmt.Errorf("request timestamp too skewed (%s)", skew.Truncate(time.Second))
	}

	canonicalRequest := buildCanonicalRequest(r, signedHeaders)
	signingKey, err := verifySigV4Signature(canonicalRequest, signature, key, dateStamp, region, service, amzDate)
	if err != nil {
		return nil, err
	}
	credScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	return streamingMaterialFromRequest(r, signature, signingKey, credScope, amzDate)
}

// parseSigV4Credential splits the SigV4 credential scope and validates the
// signed-headers list contains "host" (which the spec requires to defeat
// cross-endpoint replay). Returns the date/region/service triple plus the
// expanded header list.
func parseSigV4Credential(credential, signedHeadersStr string) (dateStamp, region, service string, signedHeaders []string, err error) {
	credParts := strings.SplitN(credential, "/", 5)
	if len(credParts) != 5 {
		return "", "", "", nil, fmt.Errorf("malformed credential scope")
	}
	signedHeaders = strings.Split(signedHeadersStr, ";")
	if !slices.Contains(signedHeaders, "host") {
		return "", "", "", nil, fmt.Errorf("host header must be signed")
	}
	return credParts[1], credParts[2], credParts[3], signedHeaders, nil
}

// parseSigV4Time parses the SigV4 amzDate (yyyymmddTHHMMSSZ) and asserts
// that its date prefix matches the credential scope's dateStamp  -  this
// prevents replaying a signing key derived for a different day.
func parseSigV4Time(amzDate, dateStamp string) (time.Time, error) {
	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed X-Amz-Date: %w", err)
	}
	if amzDate[:8] != dateStamp {
		return time.Time{}, fmt.Errorf("credential date does not match X-Amz-Date")
	}
	return reqTime, nil
}

// verifySigV4Signature computes the SigV4 stringToSign + signing key and
// compares the resulting signature to the request's signature in
// constant time. Returns the derived signing key on success so the
// caller can chain it into per-chunk validation for streaming requests.
// Shared by the header and presigned verification paths.
func verifySigV4Signature(canonicalRequest, signature string, key keyMaterial, dateStamp, region, service, amzDate string) ([]byte, error) {
	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))
	signingKey := deriveSigningKey(key.SecretAccessKey, dateStamp, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return nil, fmt.Errorf("signature mismatch")
	}
	return signingKey, nil
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
		return "", errors.New(errAuthFailed)
	}

	accessKey, _, _ := strings.Cut(credential, "/")
	if accessKey == "" {
		return "", errors.New(errAuthFailed)
	}

	entry, ok := br.byAccessKey[accessKey]
	secret := "dummy-secret-for-constant-time-auth"
	if ok {
		secret = entry.SecretAccessKey
	}

	if err := verifyPresignedSigV4(r,
		keyMaterial{AccessKeyID: accessKey, SecretAccessKey: secret, Known: ok},
		credential, signedHeaders, signature, amzDate, expires); err != nil || !ok {
		return "", errors.New(errAuthFailed)
	}

	return entry.BucketName, nil
}

// verifyPresignedSigV4 verifies a presigned URL signature. Unlike header-based
// SigV4, the date and expiry come from query parameters, the X-Amz-Signature
// parameter is excluded from the canonical query string, and the payload hash
// is always UNSIGNED-PAYLOAD.
func verifyPresignedSigV4(r *http.Request, key keyMaterial, credential, signedHeadersStr, signature, amzDate, expiresStr string) error {
	dateStamp, region, service, signedHeaders, err := parseSigV4Credential(credential, signedHeadersStr)
	if err != nil {
		return err
	}
	reqTime, err := parseSigV4Time(amzDate, dateStamp)
	if err != nil {
		return err
	}
	if err := validatePresignedExpiry(reqTime, expiresStr); err != nil {
		return err
	}

	canonicalRequest := buildPresignedCanonicalRequest(r, signedHeaders)
	_, err = verifySigV4Signature(canonicalRequest, signature, key, dateStamp, region, service, amzDate)
	return err
}

// validatePresignedExpiry rejects presigned URLs whose X-Amz-Expires value
// is malformed, exceeds the seven-day cap, or has elapsed relative to the
// presigning timestamp.
func validatePresignedExpiry(reqTime time.Time, expiresStr string) error {
	expirySecs, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || expirySecs <= 0 {
		return fmt.Errorf("invalid X-Amz-Expires value")
	}
	// Validate against the 7-day maximum before converting to time.Duration
	// to prevent integer overflow on large values.
	if expirySecs > int64(presignedMaxExpiry.Seconds()) {
		return fmt.Errorf("X-Amz-Expires exceeds maximum (%s)", presignedMaxExpiry)
	}
	if time.Now().After(reqTime.Add(time.Duration(expirySecs) * time.Second)) {
		return fmt.Errorf("presigned URL has expired")
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

	// Canonical URI - see buildCanonicalRequest comment; same wire-form
	// rule applies here for presigned URL verification.
	encodePath(&b, canonicalPath(r))
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

	writeCanonicalHeadersAndSigned(&b, r, signedHeaders)

	// Payload hash  -  always UNSIGNED-PAYLOAD for presigned URLs
	b.WriteString("UNSIGNED-PAYLOAD")

	return b.String()
}

// writeCanonicalHeadersAndSigned emits the four lines of the SigV4
// canonical request that fall between the canonical query string and
// the payload hash: each `name:value` header pair, a blank line, the
// semicolon-joined SignedHeaders list, and the closing newline.
// signedHeaders is lowercased in place so callers see the same form
// the SigV4 string-to-sign expects.
func writeCanonicalHeadersAndSigned(b *strings.Builder, r *http.Request, signedHeaders []string) {
	for i, h := range signedHeaders {
		h = strings.ToLower(stripWhitespace(h))
		signedHeaders[i] = h
		val := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && val == "" {
			val = r.Host
		}
		val = collapseWhitespace(val)
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	for i, h := range signedHeaders {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(h)
	}
	b.WriteByte('\n')
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

	// Canonical URI - the wire-form path (RawPath when set, Path
	// otherwise) preserves %XX sequences byte-for-byte so the canonical
	// request matches what AWS SDKs sign for the same URL.
	encodePath(&b, canonicalPath(r))
	b.WriteByte('\n')

	// Canonical query string
	buildCanonicalQueryString(&b, r.URL.Query())
	b.WriteByte('\n')

	writeCanonicalHeadersAndSigned(&b, r, signedHeaders)

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
	slices.Sort(params)
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

// canonicalPath returns the wire-form path for SigV4 canonicalisation. Uses
// r.URL.RawPath when non-empty (the path as it appeared on the wire,
// preserving encodings like %2F that Go's URL parser would otherwise decode
// to / in r.URL.Path), and falls back to r.URL.Path when the raw form is
// absent (legitimately omitted by url.Parse when the encoded form would
// round-trip identically to the decoded form). This is the standard Go
// pattern for "the path the client sent us." See net/url docs.
func canonicalPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	return r.URL.Path
}

// encodePath emits the canonical URI for SigV4. The input is already in
// the wire-encoded form (see canonicalPath), so this function preserves
// %XX sequences verbatim and only encodes raw bytes that the wire form
// did not already encode. Slashes stay as literal separators per the
// SigV4 S3 contract (do-not-double-encode mode used by all AWS SDKs).
func encodePath(b *strings.Builder, wirePath string) {
	if wirePath == "" {
		b.WriteByte('/')
		return
	}
	// Index-controlled rather than range: the already-encoded-triplet case
	// advances i past the two hex digits it consumed, which range-over-int
	// would ignore.
	for i := 0; i < len(wirePath); i++ {
		c := wirePath[i]
		switch {
		case c == '/':
			b.WriteByte('/')
		case c == '%' && i+2 < len(wirePath) && isHexByte(wirePath[i+1]) && isHexByte(wirePath[i+2]):
			// Already-encoded triplet  -  pass the % and its two hex
			// digits through verbatim so the byte-for-byte canonical
			// URI matches the SDK's signed form.
			b.WriteByte('%')
			b.WriteByte(wirePath[i+1])
			b.WriteByte(wirePath[i+2])
			i += 2
		case isUnreservedPathByte(c):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
}

// hexUpper is the uppercase hex alphabet used for percent-encoding output.
const hexUpper = "0123456789ABCDEF"

// isHexByte reports whether b is an ASCII hex digit.
func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// isUnreservedPathByte reports whether b is in the SigV4 path unreserved
// set (RFC 3986 unreserved: A-Z a-z 0-9 - . _ ~). Every other byte is
// percent-encoded when it appears in raw form in the wire path.
func isUnreservedPathByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '-' || b == '.' || b == '_' || b == '~':
		return true
	}
	return false
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

// collapseWhitespace replaces runs of whitespace (spaces, tabs, newlines)
// with a single space, per the SigV4 canonical header value rules.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWS := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inWS {
				b.WriteByte(' ')
				inWS = true
			}
			continue
		}
		inWS = false
		b.WriteRune(r)
	}
	return b.String()
}

// stripWhitespace removes all whitespace characters from a string.
// Used for header names which must not contain any whitespace per HTTP spec.
func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
