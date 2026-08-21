// SPDX-License-Identifier: Apache-2.0

//go:build linux

package interop

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The s3 backend, both directions, against LocalStack.
//
// borg reaches S3 through boto3 and borge through its own signer, so these rows are two
// independent implementations of Signature Version 4 and of the same flat key space. A
// signature that is subtly wrong is a 403 with no explanation; a key layout that is subtly
// wrong is a repository the other tool cannot read. Both would pass a test where one tool
// does all the writing.

const (
	s3Endpoint = "http://localhost:4566"
	s3Bucket   = "borge-test-1"
)

func requireS3Server(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the s3 interop rows in short mode")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(s3Endpoint + "/_localstack/health")
	if err != nil {
		t.Skipf("no S3 server on %s; run tests/remote/setup.sh", s3Endpoint)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Configured but not working is a fault, not an absence - the rule the sftp rows
		// arrived at after skipping silently for a while.
		t.Fatalf("%s answered %s rather than serving S3", s3Endpoint, resp.Status)
	}
}

// s3RepoURL is a repository of this test's own inside the shared bucket.
func s3RepoURL(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("s3:test:test@%s/%s/borge-interop-%s-%d",
		s3Endpoint, s3Bucket, suffix, os.Getpid())
}

// s3Env adds the credentials boto3 reads, since borg takes them from the environment as
// well as from the URL.
func (tl *tools) s3Env() []string {
	return append(tl.env(),
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_DEFAULT_REGION=us-east-1",
	)
}

func (tl *tools) runS3(bin, dir string, args ...string) (string, error) {
	tl.t.Helper()
	return tl.runWithEnvIn(bin, tl.s3Env(), dir, args...)
}

func TestS3BothDirections(t *testing.T) {
	requireS3Server(t)
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)

	for i, c := range []struct {
		name           string
		writer, reader string
	}{
		{"borg writes, borge reads", tl.borg, tl.borge},
		{"borge writes, borg reads", tl.borge, tl.borg},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := s3RepoURL(t, fmt.Sprintf("both-%d", i))
			t.Cleanup(func() { _, _ = tl.runS3(c.reader, "", "repo-delete", "-r", url, "--force") })

			if out, err := tl.runS3(c.writer, "", "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
				t.Fatalf("repo-create over s3: %v\n%s", err, out)
			}
			if out, err := tl.runS3(c.writer, "", "create", "-r", url, "one", src); err != nil {
				t.Fatalf("create over s3: %v\n%s", err, out)
			}

			out, err := tl.runS3(c.reader, "", "repo-list", "-r", url, "--format", "{archive}{NL}")
			if err != nil {
				t.Fatalf("repo-list over s3: %v\n%s", err, out)
			}
			if lastLine(out) != "one" {
				t.Errorf("the other tool lists %q over s3", lastLine(out))
			}

			dest := t.TempDir()
			if c.reader == tl.borg {
				out, err = tl.runS3(c.reader, dest, "extract", "-r", url, "one")
			} else {
				out, err = tl.runS3(c.reader, "", "extract", "-r", url, "-C", dest, "one")
			}
			if err != nil {
				t.Fatalf("extract over s3: %v\n%s", err, out)
			}
			checkTrees(t, src, filepath.Join(dest,
				filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
		})
	}
}

// TestS3SharedRepository: both tools writing into one repository in one bucket, and each
// reading and checking what the other wrote.
//
// This is where two signers and two key layouts have to agree exactly. It also runs a
// compaction, because that is the operation that moves and deletes keys rather than only
// adding them - and a move on S3 is a copy plus a delete, which is the least filesystem-like
// thing this backend does.
func TestS3SharedRepository(t *testing.T) {
	requireS3Server(t)
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)

	url := s3RepoURL(t, "shared")
	t.Cleanup(func() { _, _ = tl.runS3(tl.borge, "", "repo-delete", "-r", url, "--force") })

	if out, err := tl.runS3(tl.borg, "", "repo-create", "-r", url, "-e", "aes256-ocb"); err != nil {
		t.Fatalf("repo-create: %v\n%s", err, out)
	}
	if out, err := tl.runS3(tl.borg, "", "create", "-r", url, "by-borg", src); err != nil {
		t.Fatalf("borg create over s3: %v\n%s", err, out)
	}
	if out, err := tl.runS3(tl.borge, "", "create", "-r", url, "by-borge", src); err != nil {
		t.Fatalf("borge create over s3: %v\n%s", err, out)
	}

	for _, reader := range []struct {
		name string
		bin  string
	}{{"borg", tl.borg}, {"borge", tl.borge}} {
		out, err := tl.runS3(reader.bin, "", "repo-list", "-r", url, "--format", "{archive}{NL}")
		if err != nil {
			t.Fatalf("%s repo-list: %v\n%s", reader.name, err, out)
		}
		for _, want := range []string{"by-borg", "by-borge"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not see %q in the shared bucket:\n%s", reader.name, want, out)
			}
		}
		if out, err := tl.runS3(reader.bin, "", "check", "-r", url, "--verify-data"); err != nil {
			t.Fatalf("%s check over s3: %v\n%s", reader.name, err, out)
		}
	}

	// A soft delete and a compaction: the operations that move and remove keys.
	if out, err := tl.runS3(tl.borge, "", "delete", "-r", url, "-a", "by-borge"); err != nil {
		t.Fatalf("borge delete over s3: %v\n%s", err, out)
	}
	if out, err := tl.runS3(tl.borg, "", "compact", "-r", url); err != nil {
		t.Fatalf("borg compact over s3: %v\n%s", err, out)
	}
	// What survives still restores, read by the tool that did not write it.
	dest := t.TempDir()
	if out, err := tl.runS3(tl.borge, "", "extract", "-r", url, "-C", dest, "by-borg"); err != nil {
		t.Fatalf("extract after compaction: %v\n%s", err, out)
	}
	checkTrees(t, src, filepath.Join(dest,
		filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/"))), false)
}
