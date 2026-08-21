// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The S3 backend, against LocalStack.
//
// LocalStack is S3's API without S3, which is enough for everything here: the protocol, the
// signature, the flat key space that has to look like a tree, and the operations borg
// drives it with. What it cannot check is eventual consistency, and nothing in borge
// depends on the difference - a repository is written once and read by key.

const (
	s3TestEndpoint = "http://localhost:4566"
	s3TestBucket   = "borge-test-1"
	s3TestKeyID    = "test"
	s3TestSecret   = "test"
)

// requireS3 skips when there is no test bucket here, and fails when there is one that
// misbehaves - the same rule the sftp tests use, and for the same reason: a skip that fires
// by accident is a test reporting success while measuring nothing.
func requireS3(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the s3 backend tests in short mode")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(s3TestEndpoint + "/_localstack/health")
	if err != nil {
		t.Skipf("no S3 server on %s; run tests/remote/setup.sh (it starts LocalStack and "+
			"creates the bucket)", s3TestEndpoint)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %s rather than serving S3; that is a broken test environment "+
			"rather than a missing one", s3TestEndpoint, resp.Status)
	}
}

// s3TestURL is a prefix of this test's own inside the shared bucket.
func s3TestURL(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, t.Name())
	return fmt.Sprintf("s3:%s:%s@%s/%s/borge-test-%s-%d",
		s3TestKeyID, s3TestSecret, s3TestEndpoint, s3TestBucket, name, os.Getpid())
}

// newS3ForTest returns an opened backend over a fresh prefix, and a way to plant a key the
// backend would refuse to write.
func newS3ForTest(t *testing.T) (Backend, planter) {
	t.Helper()
	requireS3(t)
	url := s3TestURL(t)
	backend, err := NewS3(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		fresh, err := NewS3(url)
		if err == nil {
			_ = fresh.Destroy()
		}
	})
	if err := backend.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, s3Planter(backend)
}

// s3Planter puts a key straight into the bucket, bypassing the name rules.
func s3Planter(b *S3) planter {
	return func(t *testing.T, name string) {
		t.Helper()
		resp, err := b.do(http.MethodPut, b.base+name, nil, []byte("not borge's"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("planting %q answered %s", name, resp.Status)
		}
	}
}

// TestS3URLParsing over borgstore's grammar, including the two combinations it refuses.
func TestS3URLParsing(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "from-environment")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-from-environment")

	for _, c := range []struct {
		url      string
		bucket   string
		base     string
		endpoint string
		keyID    string
	}{
		{"s3:key:secret@http://localhost:4566/bucket/repo", "bucket", "repo/",
			"http://localhost:4566", "key"},
		// No endpoint: the real AWS, addressed by the bucket's own hostname.
		{"s3:key:secret@/bucket/repo/nested", "bucket", "repo/nested/", "", "key"},
		// No credentials in the URL: the environment supplies them.
		{"s3:/bucket/repo", "bucket", "repo/", "", "from-environment"},
		// b2 is the same grammar and the same backend.
		{"b2:key:secret@/bucket/repo", "bucket", "repo/", "", "key"},
		// Percent-escapes are decoded in the key and the secret, which is how a secret
		// containing a "/" or an "@" fits in a URL.
		{"s3:key:se%2Fcret@/bucket/repo", "bucket", "repo/", "", "key"},
	} {
		got, err := NewS3(c.url)
		if err != nil {
			t.Errorf("%q: %v", c.url, err)
			continue
		}
		endpoint := ""
		if got.endpoint != nil {
			endpoint = got.endpoint.String()
		}
		if got.bucket != c.bucket || got.base != c.base || endpoint != c.endpoint ||
			got.creds.AccessKeyID != c.keyID {
			t.Errorf("%q parsed as bucket=%q base=%q endpoint=%q key=%q; want %q/%q/%q/%q",
				c.url, got.bucket, got.base, endpoint, got.creds.AccessKeyID,
				c.bucket, c.base, c.endpoint, c.keyID)
		}
	}

	if got, _ := NewS3("s3:key:se%2Fcret@/bucket/repo"); got != nil && got.creds.SecretAccessKey != "se/cret" {
		t.Errorf("the secret was not unescaped: %q", got.creds.SecretAccessKey)
	}
	// A "profile" with colons in it is not ambiguous, though it looks it: borgstore's
	// profile group forbids colons, so the alternation falls through to key:secret and
	// this is an access key called "profile" with the secret "with:colons". Checked
	// against borgstore's own regex rather than reasoned about.
	if got, err := NewS3("s3:profile:with:colons@/bucket/repo"); err != nil {
		t.Errorf("a colon-bearing credential was refused: %v", err)
	} else if got.creds.AccessKeyID != "profile" || got.creds.SecretAccessKey != "with:colons" {
		t.Errorf("parsed as key=%q secret=%q, want profile/with:colons",
			got.creds.AccessKeyID, got.creds.SecretAccessKey)
	}
	for _, bad := range []string{
		"s3:/bucket",              // no path
		"s3:bucket/repo",          // no leading slash, so no bucket
		"http://host/bucket/repo", // not an s3 URL at all
	} {
		if _, err := NewS3(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestS3AddressingStyle: a custom endpoint is addressed with the bucket in the path, and
// AWS itself with the bucket in the hostname.
//
// Almost every S3-compatible server that is not S3 serves only the path form, and the ones
// that serve both accept it. Getting this the other way round produces a DNS lookup for a
// hostname that does not exist, on a server that is running perfectly.
func TestS3AddressingStyle(t *testing.T) {
	custom, err := NewS3("s3:key:secret@http://localhost:4566/bucket/repo")
	if err != nil {
		t.Fatal(err)
	}
	got := custom.requestURL("repo/config/id", nil).String()
	if got != "http://localhost:4566/bucket/repo/config/id" {
		t.Errorf("a custom endpoint addresses %q", got)
	}

	aws, err := NewS3("s3:key:secret@/bucket/repo")
	if err != nil {
		t.Fatal(err)
	}
	aws.region = "eu-west-1"
	got = aws.requestURL("repo/config/id", nil).String()
	if got != "https://bucket.s3.eu-west-1.amazonaws.com/repo/config/id" {
		t.Errorf("AWS is addressed as %q", got)
	}
}

// TestS3DeleteOfAMissingObjectIsReported: S3's own delete succeeds for a key that was never
// there, so the backend has to look first.
//
// Without the check, "delete something that is not there" would report success and the
// store above would never learn that an object it expected is gone - which is how a
// repository quietly loses the ability to notice its own damage.
func TestS3DeleteOfAMissingObjectIsReported(t *testing.T) {
	backend, _ := newS3ForTest(t)
	if err := backend.Delete("config/absent"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Delete of a missing object gave %v, want ErrObjectNotFound", err)
	}
	// And the raw operation really does succeed, which is what the check is for.
	s3 := backend.(*S3)
	resp, err := s3.do(http.MethodDelete, s3.base+"config/absent", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Errorf("S3's delete of a missing key answered %s, so the check above may be "+
			"unnecessary - which would be worth knowing", resp.Status)
	}
}

// TestS3DirectoriesAreObjects: a namespace is a zero-byte key ending in a slash, and a
// listing has to report both those and the prefixes S3 synthesises.
func TestS3DirectoriesAreObjects(t *testing.T) {
	backend, _ := newS3ForTest(t)

	if err := backend.Mkdir("archives"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	info, err := backend.Info("archives")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || !info.Directory {
		t.Errorf("after Mkdir, Info = %+v", info)
	}

	// A key two levels down makes S3 synthesise a common prefix for the level between.
	if err := backend.Store("packs/ab/cdef", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List("packs")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "ab" || !entries[0].Directory {
		t.Errorf("listing packs/ gave %+v, want the directory ab", entries)
	}

	// Rmdir refuses a namespace that still holds something, and removes an empty one.
	if err := backend.Rmdir("packs"); err == nil {
		t.Error("Rmdir removed a namespace that is not empty")
	}
	if err := backend.Rmdir("archives"); err != nil {
		t.Errorf("Rmdir of an empty namespace: %v", err)
	}
}

// TestS3MoveIsCopyThenDelete, which is not atomic and cannot be.
//
// S3 has no rename. This is recorded as a test rather than only as a comment because a move
// is how a soft delete works: a failure between the two steps leaves the object under both
// names, and borg has the same property through boto3.
func TestS3MoveIsCopyThenDelete(t *testing.T) {
	backend, _ := newS3ForTest(t)

	if err := backend.Store("archives/one", []byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Move("archives/one", "archives/one.del"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := backend.Load("archives/one", 0, -1); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("after a move the old name gave %v", err)
	}
	got, err := backend.Load("archives/one.del", 0, -1)
	if err != nil || string(got) != "body" {
		t.Errorf("the moved object is %q, %v", got, err)
	}
}

// TestS3CreateRefusesANonEmptyPrefix, as every backend does.
func TestS3CreateRefusesANonEmptyPrefix(t *testing.T) {
	backend, _ := newS3ForTest(t)
	if err := backend.Store("config/id", []byte("x")); err != nil {
		t.Fatal(err)
	}
	again, err := NewS3(s3TestURL(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Create(); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Create over an existing store gave %v, want ErrAlreadyExists", err)
	}
}

// TestS3ReportsAMissingBucketAsMissingBucket: the bucket and a key inside it are both
// missing with a 404, and saying the wrong one sends a user looking in the wrong place.
func TestS3ReportsAMissingBucketAsMissingBucket(t *testing.T) {
	requireS3(t)
	backend, err := NewS3(fmt.Sprintf("s3:%s:%s@%s/borge-no-such-bucket/repo",
		s3TestKeyID, s3TestSecret, s3TestEndpoint))
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Open(); err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	_, err = backend.Load("config/id", 0, -1)
	if !errors.Is(err, ErrDoesNotExist) {
		t.Errorf("a missing bucket gave %v, want ErrDoesNotExist", err)
	}
	if errors.Is(err, ErrObjectNotFound) {
		t.Errorf("a missing bucket was reported as a missing object: %v", err)
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("the error does not say the bucket is what is missing: %v", err)
	}
}

// TestS3ReportsARefusalUnderstandably: a wrong secret is a 403 that says nothing, so the
// message has to say what to look at.
func TestS3ReportsARefusalUnderstandably(t *testing.T) {
	requireS3(t)
	backend, err := NewS3(fmt.Sprintf("s3:%s:wrong-secret@%s/%s/whatever",
		s3TestKeyID, s3TestEndpoint, s3TestBucket))
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Open(); err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	_, err = backend.Load("config/id", 0, -1)
	if err == nil {
		t.Fatal("a wrong secret was accepted")
	}
	if !errors.Is(err, ErrPermissionDenied) && !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("a wrong secret gave %v", err)
	}
	if errors.Is(err, ErrPermissionDenied) && !strings.Contains(err.Error(), "region") {
		t.Errorf("the refusal does not mention the three things that produce it: %v", err)
	}
}
