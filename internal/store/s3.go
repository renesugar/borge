// SPDX-License-Identifier: Apache-2.0 AND BSD-3-Clause
//
// This file is a Go port of borgstore/backends/s3.py (borgstore 0.6.1, BSD 3-Clause,
// Copyright (C) 2026 Thomas Waldmann).
// Licensed under the BSD 3-Clause License, see licenses/upstream-python/borgstore.LICENSE.rst.
// Modifications and Go translation Copyright (C) 2026 The borge authors,
// licensed under the Apache License 2.0, see LICENSE.

package store

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The S3 backend, for S3 itself and for Backblaze B2.
//
// Written against the API rather than against a library: the surface borgstore uses is
// eight operations, and an SDK for them costs a dependency tree an order of magnitude
// larger than the code it replaces (PORTING_PLAN §11.5 weighed it; the decision lives in
// this file and sigv4.go and is reversible).
//
// # An object store is not a filesystem, and borg knows it
//
// There are no directories. borgstore makes them anyway - a zero-byte object whose key ends
// in "/" - because the store above needs to create a namespace, ask whether one exists and
// remove it. A listing then has two halves: the objects under a prefix, and the
// "CommonPrefixes" S3 synthesises from the keys below it.
//
// # Move is not atomic here, and cannot be
//
// S3 has no rename. A move is a copy followed by a delete, so a failure between the two
// leaves the object under both names. That matters because a move is how a soft delete
// works, and it is worth saying plainly rather than discovering: borg has exactly the same
// property through boto3 and lives with it.

const (
	// s3Delimiter is what makes a flat key space look like a tree, in listings and in the
	// keys that stand for directories.
	s3Delimiter = "/"
	// s3PageSize is how many keys one listing asks for. borgstore uses 1000, which is
	// also S3's own maximum.
	s3PageSize = 1000
	// s3TailThreshold is the same optimisation the other remote backends have.
	s3TailThreshold = 1024
	// s3Timeout bounds a single request.
	s3Timeout = 120 * time.Second
)

// s3URL is borgstore's grammar:
//
//	(s3|b2):[profile|(key:secret)@][scheme://host[:port]]/bucket/path
var s3URL = regexp.MustCompile(
	`^(s3|b2):` + // the scheme, which also says whether this is B2
		`(?:(?:([^@:]+)|([^:@]+):([^@]+))@)?` + // profile, or key and secret
		`(?:([^:/]+)://([^:/]+)(?::(\d+))?)?` + // an optional endpoint of its own
		`/([^/]+)/` + // the bucket
		`(.+)$`) // the path within it

// S3 is a Backend that stores objects in an S3 bucket.
type S3 struct {
	bucket string
	// base is the prefix every key starts with, always ending in a delimiter.
	base string

	endpoint *url.URL
	region   string
	creds    awsCredentials

	// pathStyle addresses the bucket as a path component rather than as a subdomain.
	// Required by almost every S3-compatible server that is not S3.
	pathStyle bool

	client *http.Client
	opened bool

	// now is time.Now, replaced in tests that check a signature.
	now func() time.Time
}

// NewS3 parses an s3: or b2: URL and returns a backend for it.
func NewS3(rawURL string) (*S3, error) {
	m := s3URL.FindStringSubmatch(rawURL)
	if m == nil {
		return nil, fmt.Errorf("store: %q is not an s3 URL; the form is "+
			"s3:[profile|key:secret@][scheme://host[:port]]/bucket/path", rawURL)
	}
	profile, keyID, secret := m[2], m[3], m[4]
	if profile != "" && keyID != "" {
		return nil, errors.New("store: an s3 URL names either a profile or an access key, not both")
	}
	if keyID != "" && secret == "" {
		return nil, errors.New("store: an s3 URL with an access key must also carry its secret")
	}
	keyID, err := url.PathUnescape(keyID)
	if err != nil {
		return nil, fmt.Errorf("store: the access key in %q is not valid: %w", rawURL, err)
	}
	secret, err = url.PathUnescape(secret)
	if err != nil {
		return nil, fmt.Errorf("store: the secret in %q is not valid: %w", rawURL, err)
	}
	// The bucket is not unescaped: every character a bucket name may contain is already
	// URL-safe, so an escape in one is a mistake rather than an encoding.
	bucket := m[8]
	base, err := url.PathUnescape(m[9])
	if err != nil {
		return nil, fmt.Errorf("store: the path in %q is not valid: %w", rawURL, err)
	}

	b := &S3{
		bucket: bucket,
		base:   strings.TrimSuffix(base, s3Delimiter) + s3Delimiter,
		client: &http.Client{Timeout: s3Timeout},
		now:    time.Now,
	}
	if scheme, host := m[5], m[6]; scheme != "" && host != "" {
		address := scheme + "://" + host
		if port := m[7]; port != "" {
			address += ":" + port
		}
		if b.endpoint, err = url.Parse(address); err != nil {
			return nil, fmt.Errorf("store: the endpoint in %q is not a URL: %w", rawURL, err)
		}
		// A server that is not AWS almost never serves virtual-hosted buckets, and the
		// ones that do accept the path form as well.
		b.pathStyle = true
	}
	b.region = awsRegion()
	if b.creds, err = awsResolveCredentials(profile, keyID, secret); err != nil {
		return nil, err
	}
	return b, nil
}

// awsRegion is the region requests are signed for.
//
// The region is part of the signature, so a wrong one is a 403 rather than a redirect.
// us-east-1 is the default because it is what an S3-compatible server that has no regions
// at all expects to be signed with.
func awsRegion() string {
	if region := firstEnv("AWS_REGION", "AWS_DEFAULT_REGION"); region != "" {
		return region
	}
	return "us-east-1"
}

// awsResolveCredentials finds the credentials for a request: the URL first, then the
// environment, then the named profile in the AWS credentials file.
//
// The order is boto3's, which is what borg uses, so a machine set up for one tool works for
// the other.
func awsResolveCredentials(profile, keyID, secret string) (awsCredentials, error) {
	if keyID != "" {
		return awsCredentials{AccessKeyID: keyID, SecretAccessKey: secret}, nil
	}
	if profile == "" {
		if id := os.Getenv("AWS_ACCESS_KEY_ID"); id != "" {
			return awsCredentials{
				AccessKeyID:     id,
				SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
				SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			}, nil
		}
		profile = os.Getenv("AWS_PROFILE")
		if profile == "" {
			profile = "default"
		}
	}
	creds, err := awsCredentialsFromFile(profile)
	if err != nil {
		return awsCredentials{}, err
	}
	return creds, nil
}

// awsCredentialsFromFile reads one profile out of ~/.aws/credentials.
//
// The format is an ini file, and this reads the three keys that matter and ignores the
// rest. Nothing here follows "source_profile" or assumes a role: that is a whole subsystem,
// and a user who needs it can put the resulting keys in the environment.
func awsCredentialsFromFile(profile string) (awsCredentials, error) {
	path := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if path == "" {
		path = fmt.Sprintf("%s/.aws/credentials", homeDir())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return awsCredentials{}, fmt.Errorf("store: no credentials for an s3 repository: "+
			"put them in the URL, in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or in %s: %w",
			path, err)
	}
	var creds awsCredentials
	inProfile := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProfile = strings.TrimSpace(line[1:len(line)-1]) == profile
			continue
		}
		if !inProfile {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "aws_access_key_id":
			creds.AccessKeyID = strings.TrimSpace(value)
		case "aws_secret_access_key":
			creds.SecretAccessKey = strings.TrimSpace(value)
		case "aws_session_token":
			creds.SessionToken = strings.TrimSpace(value)
		}
	}
	if creds.AccessKeyID == "" {
		return awsCredentials{}, fmt.Errorf("store: the profile %q in %s has no "+
			"aws_access_key_id", profile, path)
	}
	return creds, nil
}

// requestURL builds the URL for a key, in whichever addressing style this endpoint needs.
func (b *S3) requestURL(key string, query url.Values) *url.URL {
	scheme, host := "https", b.bucket+".s3."+b.region+".amazonaws.com"
	prefix := ""
	if b.endpoint != nil {
		scheme, host = b.endpoint.Scheme, b.endpoint.Host
	}
	if b.pathStyle {
		prefix = "/" + b.bucket
	}
	u := &url.URL{Scheme: scheme, Host: host, Path: prefix + "/" + key}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u
}

// do signs and sends one request.
func (b *S3) do(method, key string, query url.Values, body []byte, headers map[string]string) (
	*http.Response, error) {

	target := b.requestURL(key, query)
	req, err := http.NewRequest(method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	req.ContentLength = int64(len(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	signV4(req, body, b.creds, b.region, b.now())

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: %s %s: %w", method, target.Redacted(), err)
	}
	return resp, nil
}

// s3Error is the XML an S3 server answers a failure with.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// fail turns a response into an error, closing the body.
func (b *S3) fail(resp *http.Response, name string) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	var parsed s3Error
	_ = xml.Unmarshal(body, &parsed)
	switch {
	case parsed.Code == "NoSuchBucket":
		// Before the 404 below, which would otherwise swallow it: a missing bucket is
		// also answered with 404, and reporting it as "object not found" sends a user
		// looking for a key when what is missing is the bucket. Found by a test that
		// pointed at a bucket that does not exist.
		return fmt.Errorf("%w: the bucket %s does not exist", ErrDoesNotExist, b.bucket)
	case resp.StatusCode == http.StatusNotFound || parsed.Code == "NoSuchKey":
		return &ObjectNotFoundError{Name: name}
	case resp.StatusCode == http.StatusForbidden:
		// The most common failure, and the least self-explanatory: the signature is
		// computed from the credentials, the region and the request, so any of the three
		// being wrong arrives here.
		return fmt.Errorf("%w: %s refused the request (%s: %s). Check the access key, "+
			"the secret and the region", ErrPermissionDenied, b.bucket, parsed.Code, parsed.Message)
	default:
		return fmt.Errorf("store: %s: %s: %s %s", name, resp.Status, parsed.Code, parsed.Message)
	}
}

func (b *S3) requireOpen() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	return nil
}

// key turns a store name into the object key it lives under.
func (b *S3) key(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	return b.base + name, nil
}

// directoryKey is the key that stands for a namespace: the prefix, ending in a delimiter.
func (b *S3) directoryKey(name string) string {
	if name == "" {
		return b.base
	}
	return strings.TrimSuffix(b.base+name, s3Delimiter) + s3Delimiter
}

// Open and Close only mark the backend usable: S3 has no connection to hold.
func (b *S3) Open() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	b.opened = true
	return nil
}

func (b *S3) Close() error {
	if !b.opened {
		return ErrMustBeOpen
	}
	b.opened = false
	return nil
}

// Create makes the base prefix, which must be empty or absent.
func (b *S3) Create() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	listing, err := b.list(b.base, s3Delimiter, 1, "", "")
	if err != nil {
		return err
	}
	if listing.KeyCount > 0 {
		return fmt.Errorf("%w: the s3 prefix is not empty: %s", ErrAlreadyExists, b.base)
	}
	return b.putDirectory("")
}

// Destroy removes every object under the base prefix.
func (b *S3) Destroy() error {
	if b.opened {
		return ErrMustNotBeOpen
	}
	listing, err := b.list(b.base, s3Delimiter, 1, "", "")
	if err != nil {
		return err
	}
	if listing.KeyCount == 0 {
		return fmt.Errorf("%w: nothing is stored under %s", ErrDoesNotExist, b.base)
	}
	for {
		// No delimiter here: this has to see every key below the prefix, not just the
		// first level.
		page, err := b.list(b.base, "", s3PageSize, "", "")
		if err != nil {
			return err
		}
		if len(page.Contents) == 0 {
			return nil
		}
		if err := b.deleteObjects(page.Contents); err != nil {
			return err
		}
		if !page.IsTruncated {
			return nil
		}
	}
}

// deleteObjects removes a page of keys in one request, which is what makes destroying a
// large repository take seconds rather than an hour.
func (b *S3) deleteObjects(objects []s3Object) error {
	type object struct {
		Key string `xml:"Key"`
	}
	payload := struct {
		XMLName xml.Name `xml:"Delete"`
		Objects []object `xml:"Object"`
		Quiet   bool     `xml:"Quiet"`
	}{Quiet: true}
	for _, item := range objects {
		payload.Objects = append(payload.Objects, object{Key: item.Key})
	}
	body, err := xml.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// The batch delete is the one request S3 checksums rather than signs alone: without
	// Content-MD5 or a checksum header it is refused outright.
	sum := contentMD5(body)
	resp, err := b.do(http.MethodPost, "", url.Values{"delete": {""}}, body, map[string]string{
		"Content-Type": "application/xml",
		"Content-MD5":  sum,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return b.fail(resp, "delete")
	}
	resp.Body.Close()
	return nil
}

// putDirectory writes the zero-byte object that stands for a namespace.
func (b *S3) putDirectory(name string) error {
	resp, err := b.do(http.MethodPut, b.directoryKey(name), nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return b.fail(resp, name)
	}
	resp.Body.Close()
	return nil
}

// Load reads an object, or a range of one.
func (b *S3) Load(name string, offset, size int64) ([]byte, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	key, err := b.key(name)
	if err != nil {
		return nil, err
	}

	var truncateTo int64 = -1
	rangeHeader := ""
	switch {
	case offset < 0 && size >= 0:
		if -offset-size <= s3TailThreshold {
			rangeHeader = makeRangeHeader(offset, -1, -1)
			truncateTo = size
		} else {
			info, err := b.Info(name)
			if err != nil {
				return nil, err
			}
			if !info.Exists {
				return nil, &ObjectNotFoundError{Name: name}
			}
			rangeHeader = makeRangeHeader(offset, size, info.Size)
		}
	default:
		rangeHeader = makeRangeHeader(offset, size, -1)
	}
	headers := map[string]string{}
	if rangeHeader != "" {
		headers["Range"] = rangeHeader
	}

	resp, err := b.do(http.MethodGet, key, nil, nil, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, b.fail(resp, name)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", name, err)
	}
	if truncateTo >= 0 && truncateTo < int64(len(data)) {
		data = data[:truncateTo]
	}
	return data, nil
}

// Store writes an object. A put replaces whatever was there, in one request.
func (b *S3) Store(name string, value []byte) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	key, err := b.key(name)
	if err != nil {
		return err
	}
	resp, err := b.do(http.MethodPut, key, nil, value, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return b.fail(resp, name)
	}
	resp.Body.Close()
	return nil
}

// Delete removes an object permanently.
//
// The head first is not redundant: S3's delete succeeds for a key that was never there, so
// without it "delete something that does not exist" would report success and the store
// above would never learn that an object it expected is gone.
func (b *S3) Delete(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	key, err := b.key(name)
	if err != nil {
		return err
	}
	info, err := b.Info(name)
	if err != nil {
		return err
	}
	if !info.Exists {
		return &ObjectNotFoundError{Name: name}
	}
	resp, err := b.do(http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return b.fail(resp, name)
	}
	resp.Body.Close()
	return nil
}

// Move copies the object to the new name and deletes the old one.
//
// Not atomic, and it cannot be: S3 has no rename. A failure between the two leaves the
// object under both names, which for a soft delete means an object that is both live and
// deleted until something cleans up. borg has the same property.
func (b *S3) Move(oldName, newName string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	from, err := b.key(oldName)
	if err != nil {
		return err
	}
	to, err := b.key(newName)
	if err != nil {
		return err
	}
	resp, err := b.do(http.MethodPut, to, nil, nil, map[string]string{
		// The source is a header, and it names the bucket as well as the key.
		"x-amz-copy-source": "/" + b.bucket + "/" + from,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return b.fail(resp, oldName)
	}
	resp.Body.Close()

	resp, err = b.do(http.MethodDelete, from, nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return b.fail(resp, oldName)
	}
	resp.Body.Close()
	return nil
}

// Info reports on one name without reading it.
func (b *S3) Info(name string) (ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return ItemInfo{}, err
	}
	key, err := b.key(name)
	if err != nil {
		return ItemInfo{}, err
	}
	resp, err := b.do(http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return ItemInfo{}, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		return ItemInfo{
			Name:   path.Base(key),
			Exists: true,
			Size:   size,
			MTime:  parseHTTPTime(resp.Header.Get("Last-Modified")),
		}, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return ItemInfo{}, b.fail(resp, name)
	}
	// Not an object - but it may be a namespace, which is an object whose key ends in a
	// delimiter.
	resp, err = b.do(http.MethodHead, b.directoryKey(name), nil, nil, nil)
	if err != nil {
		return ItemInfo{}, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return ItemInfo{Name: path.Base(key), Exists: true, Directory: true}, nil
	}
	return ItemInfo{Name: path.Base(key)}, nil
}

// s3Object is one key in a listing.
type s3Object struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

// s3Listing is the answer to list-objects-v2.
type s3Listing struct {
	XMLName        xml.Name   `xml:"ListBucketResult"`
	KeyCount       int        `xml:"KeyCount"`
	IsTruncated    bool       `xml:"IsTruncated"`
	NextToken      string     `xml:"NextContinuationToken"`
	Contents       []s3Object `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// list runs one page of list-objects-v2.
func (b *S3) list(prefix, delimiter string, maxKeys int, startAfter, token string) (*s3Listing, error) {
	query := url.Values{
		"list-type": {"2"},
		"prefix":    {prefix},
		"max-keys":  {strconv.Itoa(maxKeys)},
	}
	if delimiter != "" {
		query.Set("delimiter", delimiter)
	}
	if startAfter != "" {
		query.Set("start-after", startAfter)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	// The bucket itself is the target of a listing, so the key is empty.
	resp, err := b.do(http.MethodGet, "", query, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, b.fail(resp, prefix)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var listing s3Listing
	if err := xml.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("store: the listing of %s was not XML this can read: %w", prefix, err)
	}
	return &listing, nil
}

// List reports one namespace's entries, sorted by name.
//
// Both halves of a listing appear here: the objects directly under the prefix, and the
// "common prefixes" S3 reports for keys further down, which are the directories.
func (b *S3) List(name string) ([]ItemInfo, error) {
	if err := b.requireOpen(); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	prefix := b.directoryKey(name)

	var out []ItemInfo
	token := ""
	found := false
	for {
		page, err := b.list(prefix, s3Delimiter, s3PageSize, "", token)
		if err != nil {
			return nil, err
		}
		if page.KeyCount > 0 {
			found = true
		}
		for _, item := range page.Contents {
			entry := strings.TrimPrefix(item.Key, prefix)
			if entry == "" {
				// The object that stands for this namespace itself.
				continue
			}
			if validateName(entry) != nil {
				continue
			}
			out = append(out, ItemInfo{
				Name:   entry,
				Exists: true,
				Size:   item.Size,
				MTime:  parseS3Time(item.LastModified),
			})
		}
		for _, item := range page.CommonPrefixes {
			entry := strings.TrimSuffix(strings.TrimPrefix(item.Prefix, prefix), s3Delimiter)
			if entry == "" || validateName(entry) != nil {
				continue
			}
			out = append(out, ItemInfo{Name: entry, Exists: true, Directory: true})
		}
		if !page.IsTruncated || page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	if !found {
		return nil, &ObjectNotFoundError{Name: name}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Mkdir creates a namespace.
func (b *S3) Mkdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	return b.putDirectory(name)
}

// Rmdir removes an empty namespace.
func (b *S3) Rmdir(name string) error {
	if err := b.requireOpen(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	prefix := b.directoryKey(name)
	page, err := b.list(prefix, s3Delimiter, 2, "", "")
	if err != nil {
		return err
	}
	if page.KeyCount == 0 {
		return &ObjectNotFoundError{Name: name}
	}
	// One key is the namespace object itself; anything else means it is not empty.
	if len(page.Contents) > 1 || len(page.CommonPrefixes) > 0 {
		return fmt.Errorf("store: the namespace %s is not empty", name)
	}
	resp, err := b.do(http.MethodDelete, prefix, nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return b.fail(resp, name)
	}
	resp.Body.Close()
	return nil
}

// parseS3Time reads the ISO 8601 timestamp a listing carries.
func parseS3Time(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// parseHTTPTime reads the Last-Modified header, which is in HTTP's own format rather than
// the one the listing uses.
func parseHTTPTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
