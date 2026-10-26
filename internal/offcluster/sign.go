package offcluster

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// emptyHash is the SHA-256 of no payload, which a request without a body signs.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials are the keys a destination is written with.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// sign adds AWS Signature Version 4 to req for the S3 service, over payloadHash (the hex
// SHA-256 of the body), host and every header req already carries.
func sign(req *http.Request, payloadHash, region string, credentials Credentials, at time.Time) {
	stamp := at.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		escapePath(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexHash([]byte(canonical))

	key := mac([]byte("AWS4"+credentials.SecretKey), day)
	for _, part := range []string{region, "s3", "aws4_request"} {
		key = mac(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+credentials.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(mac(key, toSign)))
}

// escapePath is the path S3 signs: each segment escaped once, as the request sends it.
func escapePath(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		raw, err := url.PathUnescape(segment)
		if err != nil {
			raw = segment
		}
		segments[i] = escape(raw)
	}
	return strings.Join(segments, "/")
}

func canonicalQuery(query url.Values) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var pairs []string
	for _, key := range keys {
		values := append([]string(nil), query[key]...)
		sort.Strings(values)
		for _, value := range values {
			pairs = append(pairs, escape(key)+"="+escape(value))
		}
	}
	return strings.Join(pairs, "&")
}

// escape is SigV4's URI encoding: every byte but the unreserved ones, as %XX in upper case.
func escape(value string) string {
	var out strings.Builder
	for _, b := range []byte(value) {
		if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || strings.IndexByte("-._~", b) >= 0 {
			out.WriteByte(b)
			continue
		}
		out.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{b})))
	}
	return out.String()
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
