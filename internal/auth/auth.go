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

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
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

	// Legacy token auth
	proxyToken := r.Header.Get("X-Proxy-Token")
	if proxyToken != "" {
		for token, bucket := range br.byToken {
			if subtle.ConstantTimeCompare([]byte(proxyToken), []byte(token)) == 1 {
				return bucket, nil
			}
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
	fields := parseSigV4Fields(parts)

	credential := fields["Credential"]
	signedHeadersStr := fields["SignedHeaders"]
	signature := fields["Signature"]

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

	// Derive signing key
	signingKey := deriveSigningKey(secretAccessKey, dateStamp, region, service)

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
func parseSigV4Fields(s string) map[string]string {
	fields := make(map[string]string)
	for _, part := range strings.Split(s, ", ") {
		part = strings.TrimSpace(part)
		idx := strings.IndexByte(part, '=')
		if idx > 0 {
			fields[part[:idx]] = part[idx+1:]
		}
	}
	return fields
}

// buildCanonicalRequest constructs the canonical request string per SigV4 spec.
func buildCanonicalRequest(r *http.Request, signedHeaders []string) string {
	// Canonical URI
	canonicalURI := r.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	// Canonical query string
	canonicalQueryString := buildCanonicalQueryString(r.URL.Query())

	// Canonical headers
	var headerLines []string
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
