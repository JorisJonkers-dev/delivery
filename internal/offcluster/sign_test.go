package offcluster

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The examples AWS publishes for Signature Version 4 on S3, signed with its example keys
// ("Signature Calculations for the Authorization Header: Transferring Payload in a Single Chunk").
func TestTheSignatureIsAWSsOwnForItsPublishedExamples(t *testing.T) {
	example := Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"} //nolint:gosec // G101: AWS's published example keys
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		method, target, payload string
		headers                 map[string]string
		signed, signature       string
	}{
		"get an object": {
			http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", emptyHash,
			map[string]string{"Range": "bytes=0-9"},
			"host;range;x-amz-content-sha256;x-amz-date",
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		"put an object whose name escapes": {
			http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text",
			"44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		"a query with no value": {
			http.MethodGet, "https://examplebucket.s3.amazonaws.com/?lifecycle", emptyHash, nil,
			"host;x-amz-content-sha256;x-amz-date",
			"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		"list a bucket": {
			http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", emptyHash, nil,
			"host;x-amz-content-sha256;x-amz-date",
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), c.method, c.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range c.headers {
				req.Header.Set(key, value)
			}

			sign(req, c.payload, "us-east-1", example, at)

			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
				"SignedHeaders=" + c.signed + ", Signature=" + c.signature
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestAPathSegmentIsEscapedOnceAsSigned(t *testing.T) {
	cases := map[string]string{
		"":                 "/",
		"/":                "/",
		"/a/b c/d+e":       "/a/b%20c/d%2Be",
		"/x%2Fy":           "/x%2Fy",
		"/bad%zz/ok~-._":   "/bad%25zz/ok~-._",
		"/caf%C3%A9/s%24p": "/caf%C3%A9/s%24p",
	}
	for path, want := range cases {
		if got := escapePath(path); got != want {
			t.Errorf("escapePath(%q) = %q, want %q", path, got, want)
		}
	}
	if got := canonicalQuery(map[string][]string{"b": {"2", "1"}, "a b": {"x/y"}}); got != "a%20b=x%2Fy&b=1&b=2" {
		t.Errorf("canonicalQuery = %q", got)
	}
	if strings.Contains(escape("AZaz09-._~"), "%") {
		t.Error("an unreserved byte was escaped")
	}
}
