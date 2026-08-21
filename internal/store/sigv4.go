// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Signature Version 4, which is what an S3 request has instead of a password.
//
// This is roughly two hundred lines against a dependency tree several hundred times its
// size (PORTING_PLAN §11.5 weighed the two and chose this), and it is small because the
// surface borge uses is small: eight operations, no streaming signatures, no chunked
// uploads, no presigned URLs. What it must be is exactly right - a signature that is wrong
// by one byte is a 403 with no explanation of which byte - so it is checked against
// botocore, which is the signer borg uses, over a corpus of requests
// (TestSigV4MatchesBotocore).
//
// The specification is AWS's "Signature Version 4 signing process". The parts that catch
// people out are marked below; each of them is a thing that produces a working signature
// for simple requests and a 403 for real ones.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	// sigV4Service is "s3" for every request borge makes. The service name is part of the
	// signing key, so it is not decoration.
	sigV4Service = "s3"
	// emptyPayloadHash is the SHA-256 of nothing, which every request without a body uses.
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// awsCredentials are what a request is signed with.
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// signV4 signs a request in place: it adds the headers the signature covers and the
// Authorization header itself.
//
// payload is the body as it will be sent. It is hashed, not streamed: every object borge
// stores is already in memory, and signing the payload rather than declaring it unsigned is
// what lets a proxy or a bucket policy insist on integrity.
func signV4(req *http.Request, payload []byte, creds awsCredentials, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := emptyPayloadHash
	if len(payload) > 0 {
		sum := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(sum[:])
	}

	// These three are signed, so they have to be set before the canonical request is
	// built rather than after.
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, sigV4Service, "aws4_request"}, "/")
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(requestHash[:]),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, dateStamp, region), stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, creds.AccessKeyID, scope, signedHeaders, signature))
}

// signingKey derives the key for one day, one region and one service.
//
// The nesting is the point of it: a key that leaks is useless tomorrow, in another region,
// or for another service.
func signingKey(secret, dateStamp, region string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, sigV4Service)
	return hmacSHA256(key, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// canonicalURI is the path, encoded once.
//
// S3 is the exception to SigV4's rule: every other service encodes the path twice, and S3
// does not. Getting this wrong signs a different request than the one that is sent, and the
// only symptom is a 403 on keys that contain a character needing an escape.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery is the query string with the parameters sorted and encoded.
//
// A parameter with no value keeps its "=", which url.Values.Encode does; a bare "?delete"
// therefore signs as "delete=".
func canonicalQuery(u *url.URL) string {
	values := u.Query()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var parts []string
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, item := range items {
			parts = append(parts, awsEscape(key)+"="+awsEscape(item))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape is RFC 3986 escaping, which differs from Go's in the two characters that matter
// here: a space is %20 rather than "+", and a tilde is left alone.
func awsEscape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// canonicalHeaders builds the signed header block and the list of names it covers.
//
// Only the headers listed in SignedHeaders are covered, and every one of them must be in
// the block, lowercased, in order, with runs of whitespace in the value collapsed.
func canonicalHeaders(req *http.Request) (string, string) {
	names := make([]string, 0, len(req.Header)+1)
	values := map[string]string{}
	for name, list := range req.Header {
		lower := strings.ToLower(name)
		switch lower {
		case "authorization", "content-length", "user-agent":
			// Not signed: Authorization is what is being produced, and the other two are
			// set or rewritten by the transport after signing.
			continue
		}
		names = append(names, lower)
		collapsed := make([]string, len(list))
		for i, value := range list {
			collapsed[i] = strings.Join(strings.Fields(value), " ")
		}
		values[lower] = strings.Join(collapsed, ",")
	}
	sort.Strings(names)

	var block strings.Builder
	for _, name := range names {
		block.WriteString(name)
		block.WriteString(":")
		block.WriteString(values[name])
		block.WriteString("\n")
	}
	return block.String(), strings.Join(names, ";")
}

// contentMD5 is the base64 MD5 an S3 batch delete must carry.
//
// MD5 is not a security choice here and is not used as one: S3 requires this exact header
// on a multi-object delete, and refuses the request without it. The signature is what
// authenticates the request; this is a shape the protocol insists on.
func contentMD5(body []byte) string {
	sum := md5.Sum(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}
