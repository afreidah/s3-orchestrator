// -------------------------------------------------------------------------------
// Authentication - AWS Signature Version 4 Verification
//
// Author: Alex Freidah
//
// Implements AWS SigV4 signature verification for S3 client compatibility. Parses
// the Authorization header, reconstructs the canonical request, and verifies the
// HMAC-SHA256 signature chain. Also supports legacy X-Proxy-Token authentication
// for backward compatibility with simple clients.
//
// BucketRegistry maps client credentials to virtual buckets, enabling multi-tenant
// access with per-bucket credential isolation.
// -------------------------------------------------------------------------------

// Package auth provides S3 SigV4 and token-based request authentication with
// multi-bucket credential resolution.
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
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// sigV4MaxSkew is the maximum allowed clock skew for SigV4 request timestamps.
const sigV4MaxSkew = 15 * time.Minute

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
	byAccessKey map[string]bucketEntry // access_key_id -> {bucket, secret}
	byToken     map[string]string      // token -> bucket name
}

// NewBucketRegistry builds a credential-to-bucket lookup from the config.
func NewBucketRegistry(buckets []config.BucketConfig) *BucketRegistry {
	br := &BucketRegistry{
		byAccessKey: make(map[string]bucketEntry),
		byToken:     make(map[string]string),
	}

	for _, bkt := range buckets {
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

// AuthenticateAndResolveBucket authenticates the request and returns the
// authorized bucket name. Returns an error if authentication fails.
func (br *BucketRegistry) AuthenticateAndResolveBucket(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")

	// SigV4 takes precedence
	if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
		accessKey, err := extractAccessKey(authHeader)
		if err != nil {
			return "", err
		}

		entry, ok := br.byAccessKey[accessKey]
		if !ok {
			return "", fmt.Errorf("unknown access key")
		}

		if err := VerifySigV4(r, accessKey, entry.SecretAccessKey); err != nil {
			return "", err
		}

		return entry.BucketName, nil
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

// extractAccessKey parses the access key ID from a SigV4 Authorization header.
func extractAccessKey(authHeader string) (string, error) {
	parts := strings.TrimPrefix(authHeader, "AWS4-HMAC-SHA256 ")
	fields := parseSigV4Fields(parts)

	credential := fields["Credential"]
	if credential == "" {
		return "", fmt.Errorf("malformed Authorization header")
	}

	credParts := strings.SplitN(credential, "/", 2)
	if len(credParts) < 1 || credParts[0] == "" {
		return "", fmt.Errorf("malformed credential scope")
	}

	return credParts[0], nil
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

func getCachedSigningKey(accessKeyID, secret, dateStamp, region, service string) []byte {
	if v, ok := signingKeyCache.Load(accessKeyID); ok {
		c := v.(*cachedSigningKey)
		if c.dateStamp == dateStamp && c.region == region && c.service == service {
			return c.key
		}
	}
	key := deriveSigningKey(secret, dateStamp, region, service)
	signingKeyCache.Store(accessKeyID, &cachedSigningKey{
		dateStamp: dateStamp,
		region:    region,
		service:   service,
		key:       key,
	})
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

	// Parse: AWS4-HMAC-SHA256 Credential=.../date/region/service/aws4_request, SignedHeaders=..., Signature=...
	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
		return fmt.Errorf("unsupported auth scheme")
	}

	parts := strings.TrimPrefix(authHeader, "AWS4-HMAC-SHA256 ")
	credential, signedHeadersStr, signature := parseSigV4FieldsDirect(parts)
	if credential == "" || signedHeadersStr == "" || signature == "" {
		return fmt.Errorf("malformed Authorization header")
	}

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
	hostSigned := slices.Contains(signedHeaders, "host")
	if !hostSigned {
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

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate,
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Derive signing key (cached — changes only when dateStamp rolls over)
	signingKey := getCachedSigningKey(accessKeyID, secretAccessKey, dateStamp, region, service)

	// Calculate expected signature
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
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
	// Canonical URI — each path segment must be URI-encoded per SigV4 spec.
	// Go's net/http decodes percent-encoding in r.URL.Path, so we re-encode
	// each segment to match what the client signed.
	canonicalURI := encodePath(r.URL.Path)

	// Canonical query string
	canonicalQueryString := buildCanonicalQueryString(r.URL.Query())

	// Canonical headers
	headerLines := make([]string, 0, len(signedHeaders))
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		val := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && val == "" {
			val = r.Host
		}
		headerLines = append(headerLines, h+":"+val+"\n")
	}

	canonicalHeaders := strings.Join(headerLines, "")
	signedHeadersJoined := strings.Join(signedHeaders, ";")

	// Payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		r.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeadersJoined,
		payloadHash,
	)
}

// buildCanonicalQueryString sorts query parameters and URI-encodes them per
// the SigV4 spec (RFC 3986 encoding where spaces become %20, not +).
func buildCanonicalQueryString(values url.Values) string {
	if len(values) == 0 {
		return ""
	}

	var params []string
	for k, vs := range values {
		for _, v := range vs {
			params = append(params, sigV4Encode(k)+"="+sigV4Encode(v))
		}
	}
	sort.Strings(params)
	return strings.Join(params, "&")
}

// sigV4Encode performs URI encoding per the SigV4 spec: RFC 3986 unreserved
// characters (A-Z, a-z, 0-9, '-', '.', '_', '~') are not encoded, everything
// else is percent-encoded. Unlike url.QueryEscape, spaces become %20 not +.
func sigV4Encode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// encodePath URI-encodes each path segment per the SigV4 spec. Slashes are
// preserved as literal separators; everything else follows RFC 3986.
func encodePath(rawPath string) string {
	if rawPath == "" || rawPath == "/" {
		return "/"
	}
	segments := strings.Split(rawPath, "/")
	for i, seg := range segments {
		segments[i] = sigV4Encode(seg)
	}
	return strings.Join(segments, "/")
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
