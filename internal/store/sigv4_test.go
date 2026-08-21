// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The signature, against botocore - which is the signer borg uses.
//
// A signature is all-or-nothing and says nothing about why: one byte wrong in the canonical
// request and the server answers 403 with "SignatureDoesNotMatch", the same as a wrong
// secret. Whether it works against LocalStack is proved by the backend tests; what this
// pins is the parts that only show up on requests that are not simple - a key with a space
// in it, a query with several parameters, a header with odd whitespace - where a signer can
// be wrong for years and only fail on the one object whose name has a character in it.

type sigV4Case struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// botocoreSign signs each case with botocore and returns what it produced.
func botocoreSign(t *testing.T, cases []sigV4Case) []map[string]string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the SigV4 differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the SigV4 differential test")
	}

	const script = `
import json, sys
from botocore.auth import S3SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials

creds = Credentials("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
out = []
for case in json.load(sys.stdin):
    request = AWSRequest(
        method=case["method"],
        url=case["url"],
        data=case["body"].encode() if case["body"] else b"",
        headers=dict(case["headers"] or {}),
    )
    # S3SigV4Auth, not the generic SigV4Auth: they differ on the one rule that matters
    # here, and S3's is the one boto3 uses for S3. It also adds X-Amz-Content-SHA256
    # itself, as borge does.
    S3SigV4Auth(creds, "s3", "us-east-1").add_auth(request)
    out.append({
        "amzdate": request.headers["X-Amz-Date"],
        "authorization": request.headers["Authorization"],
    })
json.dump(out, sys.stdout)
`
	input, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("botocore could not sign: %v", err)
	}
	var parsed []map[string]string
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("botocore's answer is not JSON: %v\n%s", err, out)
	}
	return parsed
}

// TestSigV4MatchesBotocore over the request shapes borge actually makes, and the ones that
// break a naive signer.
func TestSigV4MatchesBotocore(t *testing.T) {
	cases := []sigV4Case{
		{
			Name: "a plain get", Method: "GET",
			URL: "http://localhost:4566/borge-test-1/repo/config/id",
		},
		{
			Name: "a put with a body", Method: "PUT",
			URL:  "http://localhost:4566/borge-test-1/repo/config/id",
			Body: "0123456789abcdef",
		},
		{
			// Several query parameters, which have to be sorted and escaped.
			Name: "a listing", Method: "GET",
			URL: "http://localhost:4566/borge-test-1?list-type=2&prefix=repo%2Fpacks%2F" +
				"&delimiter=%2F&max-keys=1000",
		},
		{
			// A parameter with no value: "?delete" signs as "delete=".
			Name: "a batch delete", Method: "POST",
			URL:     "http://localhost:4566/borge-test-1?delete=",
			Body:    "<Delete><Object><Key>repo/config/id</Key></Object></Delete>",
			Headers: map[string]string{"Content-Type": "application/xml"},
		},
		{
			// A key with characters that need escaping. This is the case that found the
			// difference between the two signers botocore has: the generic SigV4Auth
			// encodes the path *twice* and S3SigV4Auth encodes it once, and S3 wants it
			// once. A test against the wrong one of the two would have demanded the bug.
			Name: "a key with a space and a plus", Method: "GET",
			URL: "http://localhost:4566/borge-test-1/repo/odd%20name%2Bplus",
		},
		{
			// A header whose value has runs of whitespace, which are collapsed before
			// signing.
			Name: "a header with odd whitespace", Method: "PUT",
			URL:     "http://localhost:4566/borge-test-1/repo/archives/one",
			Headers: map[string]string{"X-Amz-Meta-Comment": "two   spaces   inside"},
		},
		{
			// The copy that a move is made of: the source is a header.
			Name: "a copy", Method: "PUT",
			URL:     "http://localhost:4566/borge-test-1/repo/archives/one.del",
			Headers: map[string]string{"x-amz-copy-source": "/borge-test-1/repo/archives/one"},
		},
	}

	signed := botocoreSign(t, cases)
	if len(signed) != len(cases) {
		t.Fatalf("botocore signed %d of %d cases", len(signed), len(cases))
	}

	creds := awsCredentials{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	for i, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// botocore picks the timestamp, so borge signs for the same instant.
			when, err := time.Parse("20060102T150405Z", signed[i]["amzdate"])
			if err != nil {
				t.Fatalf("botocore's X-Amz-Date %q: %v", signed[i]["amzdate"], err)
			}
			req, err := http.NewRequest(c.Method, c.URL, strings.NewReader(c.Body))
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = int64(len(c.Body))
			for name, value := range c.Headers {
				req.Header.Set(name, value)
			}
			signV4(req, []byte(c.Body), creds, "us-east-1", when)

			got := req.Header.Get("Authorization")
			want := signed[i]["authorization"]
			if got != want {
				t.Errorf("signatures differ.\nbotocore: %s\nborge:    %s", want, got)
			}
		})
	}
}

// TestSigV4CorpusIsNotVacuous: a corpus of identical simple GETs would agree with botocore
// while exercising none of the parts that go wrong.
func TestSigV4CorpusIsNotVacuous(t *testing.T) {
	// The escaping rule that differs from Go's own.
	if got := awsEscape("a b~c/d"); got != "a%20b~c%2Fd" {
		t.Errorf("awsEscape(%q) = %q; a space must be %%20, a tilde must survive, a slash "+
			"in a query value must be escaped", "a b~c/d", got)
	}
	// A valueless query parameter keeps its "=".
	req, err := http.NewRequest("POST", "http://host/bucket?delete", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalQuery(req.URL); got != "delete=" {
		t.Errorf("canonicalQuery of \"?delete\" = %q, want %q", got, "delete=")
	}
	// Authorization and Content-Length are never signed: one is the output, the other is
	// rewritten by the transport.
	req.Header.Set("Authorization", "something")
	req.Header.Set("Content-Length", "17")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	_, signedHeaders := canonicalHeaders(req)
	for _, name := range []string{"authorization", "content-length"} {
		if strings.Contains(signedHeaders, name) {
			t.Errorf("%q is in the signed headers: %q", name, signedHeaders)
		}
	}
}
