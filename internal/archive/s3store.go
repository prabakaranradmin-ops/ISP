package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ── S3-compatible object storage (FR-NET-008) ───────────────────────────────

// S3Store writes archived documents to an S3-compatible bucket.
//
// This is the off-machine copy FR-NET-008 asks for, and the reason LocalStore
// is not enough: a copy on the same disk as the database is not disaster
// recovery, which LocalStore's own doc comment says plainly. The requirement
// sat parked because it needs a bucket and credentials, not because it needed
// designing — Store has been the seam all along.
//
// Any S3-compatible endpoint works: AWS S3, Wasabi, Backblaze B2, MinIO, or a
// appliance in the same rack. Only the endpoint and credentials differ.
type S3Store struct {
	client *minio.Client
	bucket string
	// prefix is an optional key prefix, so one bucket can hold several
	// deployments without their keys colliding.
	prefix string
	// maxBytes caps what a single Put will buffer. See Put for why there is a
	// buffer at all.
	maxBytes int64
}

var _ Store = (*S3Store)(nil)

// S3Config is what an operator supplies to turn this on.
type S3Config struct {
	// Endpoint is host[:port], without a scheme — "s3.amazonaws.com",
	// "s3.eu-central-1.wasabisys.com", "minio.internal:9000".
	Endpoint string
	Bucket   string
	// Prefix is optional and may be empty.
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	// Region matters to AWS and is ignored by most others.
	Region string
	// UseSSL should only be false against an endpoint on a trusted network.
	// These are KYC scans and invoices; in transit they deserve TLS.
	//
	// It also changes how the body is signed: over plain HTTP the client uses
	// streaming signature v4, which frames the payload in aws-chunked, while
	// over TLS it sends the payload unsigned. Anything inspecting the request
	// has to account for that.
	UseSSL bool
	// Transport overrides the HTTP transport. Nil takes the default. An
	// operator behind a corporate proxy or a private CA needs this, and the
	// tests use it to reach a server with a self-signed certificate.
	Transport http.RoundTripper
	// MaxObjectBytes caps a single document. Zero takes the default.
	MaxObjectBytes int64
}

// defaultMaxObjectBytes bounds the in-memory buffer in Put. Generous for the
// documents this archives — invoice PDFs and KYC scans are measured in
// hundreds of kilobytes — and small enough that a malformed caller cannot
// exhaust the process.
const defaultMaxObjectBytes = 64 << 20 // 64 MiB

// ErrS3NotConfigured reports that no bucket was given, so there is nothing to
// construct. Distinct from a connection failure: one is a deployment that has
// not enabled off-machine archival, the other is one that has and is broken.
var ErrS3NotConfigured = errors.New("archive: no S3 bucket configured")

// NewS3Store builds a store against cfg.
//
// It deliberately does not probe the bucket. Construction happens during
// start-up, and a daemon that refused to boot because object storage was
// briefly unreachable would trade a degraded archive for no service at all —
// the same reasoning that makes ARCHIVE_DIR optional rather than fatal. A
// broken bucket surfaces on the first Put, which is logged and retried.
func NewS3Store(cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, ErrS3NotConfigured
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("archive: S3 bucket %q given with no endpoint", cfg.Bucket)
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:    cfg.UseSSL,
		Region:    cfg.Region,
		Transport: cfg.Transport,
	})
	if err != nil {
		return nil, fmt.Errorf("archive: S3 client for %q: %w", cfg.Endpoint, err)
	}

	maxBytes := cfg.MaxObjectBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxObjectBytes
	}
	return &S3Store{
		client:   client,
		bucket:   cfg.Bucket,
		prefix:   strings.Trim(cfg.Prefix, "/"),
		maxBytes: maxBytes,
	}, nil
}

// Backend implements Store.
func (s *S3Store) Backend() string { return BackendS3 }

// objectKey applies the prefix.
func (s *S3Store) objectKey(key string) string {
	k := strings.TrimLeft(key, "/")
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

// Put streams r to the bucket.
//
// The body is buffered before the upload because Store's contract requires the
// size and checksum *as written*, and S3 wants a length up front for a
// single-request PUT. Streaming with an unknown length would mean a multipart
// upload, which leaves parts behind on an interrupted write — the exact
// partial-object visibility the interface forbids. The buffer is capped
// (maxBytes) so the bound is explicit rather than a hope about caller
// behaviour. Every caller today already holds the document in memory.
//
// A repeated Put with the same key overwrites, which is what makes a retry
// after a network failure safe: S3 makes an object visible only once its PUT
// completes, so there is no partial state to clean up.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) (*PutResult, error) {
	hasher := sha256.New()
	var buf bytes.Buffer
	// maxBytes+1 so an object exactly at the limit is accepted and one byte
	// over is detected rather than silently truncated.
	size, err := io.Copy(io.MultiWriter(&buf, hasher),
		io.LimitReader(readerWithContext(ctx, r), s.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("archive: read %q for upload: %w", key, err)
	}
	if size == 0 {
		// Same refusal as LocalStore: the schema requires size_bytes > 0, and
		// an empty object is a successful-looking record of nothing.
		return nil, fmt.Errorf("archive: refusing to archive an empty document for %q", key)
	}
	if size > s.maxBytes {
		return nil, fmt.Errorf("archive: %q is larger than the %d-byte limit for a single archived document",
			key, s.maxBytes)
	}

	objectKey := s.objectKey(key)
	if _, err := s.client.PutObject(ctx, s.bucket, objectKey,
		bytes.NewReader(buf.Bytes()), size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return nil, fmt.Errorf("archive: upload %q to s3://%s/%s: %w", key, s.bucket, objectKey, err)
	}

	return &PutResult{
		URL:            "s3://" + s.bucket + "/" + objectKey,
		SizeBytes:      size,
		ChecksumSHA256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// objectFromURL parses an s3:// URL and checks it belongs to this store.
//
// Re-validated rather than trusted, exactly as LocalStore re-validates a path:
// storage_url comes back out of the database, and a purge or a restore acting
// on whatever a tampered row pointed at could read or delete an object in
// somebody else's bucket.
func (s *S3Store) objectFromURL(url string) (string, error) {
	const scheme = "s3://"
	if !strings.HasPrefix(url, scheme) {
		return "", fmt.Errorf("archive: %q is not an s3 URL", url)
	}
	rest := strings.TrimPrefix(url, scheme)
	bucket, key, found := strings.Cut(rest, "/")
	if !found || key == "" {
		return "", fmt.Errorf("archive: s3 URL %q names no object", url)
	}
	if bucket != s.bucket {
		return "", fmt.Errorf("archive: s3 URL %q is in bucket %q, not this store's %q",
			url, bucket, s.bucket)
	}
	if s.prefix != "" && !strings.HasPrefix(key, s.prefix+"/") {
		return "", fmt.Errorf("archive: s3 URL %q is outside this store's prefix %q", url, s.prefix)
	}
	return key, nil
}

// Get opens the object at url.
func (s *S3Store) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	key, err := s.objectFromURL(url)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("archive: open %s: %w", url, err)
	}
	// minio's GetObject is lazy: it returns before the request is made, so a
	// missing object would otherwise surface as a read error much later, inside
	// whatever is copying the bytes. Stat forces it now, so "cannot be read"
	// is reported from the method whose job that is.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close() //nolint:errcheck // already failing; the close error adds nothing
		return nil, fmt.Errorf("archive: open %s: %w", url, err)
	}
	return obj, nil
}

// Delete removes the object at url.
//
// An object that is already gone is not an error, per Store's contract: the
// purge scanner retries, and a backend that failed for something it had
// already removed would retry for ever.
func (s *S3Store) Delete(ctx context.Context, url string) error {
	key, err := s.objectFromURL(url)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if isS3NotFound(err) {
			return nil
		}
		return fmt.Errorf("archive: delete %s: %w", url, err)
	}
	return nil
}

// isS3NotFound reports whether err is S3 saying the object or bucket is not
// there. Checked by code rather than by message: the wording differs between
// AWS, MinIO and every other implementation, and matching on text would make
// the purge scanner's behaviour depend on the vendor.
func isS3NotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return true
		}
		return resp.StatusCode == http.StatusNotFound
	}
	return false
}

// ── Choosing a backend ──────────────────────────────────────────────────────

// NewStore returns the archival backend a deployment has configured, or
// (nil, nil) when it has configured none.
//
// S3 wins when a bucket is set. A copy on the same disk as the database is
// not disaster recovery — LocalStore's own doc comment says so — and an
// operator who has gone to the trouble of providing a bucket means it.
//
// (nil, nil) rather than an error for "nothing configured": archival is
// optional by design, and both daemons already degrade to a Documents screen
// that says so rather than refusing to start.
//
// Exists so cmd/api and cmd/radiusd cannot drift. They had the same four
// lines of local-store setup each, and a second backend would have made that
// the same twelve.
func NewStore(localDir string, s3 S3Config) (Store, error) {
	if strings.TrimSpace(s3.Bucket) != "" {
		return NewS3Store(s3)
	}
	if strings.TrimSpace(localDir) != "" {
		return NewLocalStore(localDir)
	}
	return nil, nil
}

// Describe names the configured backend for a start-up log line, so an
// operator can see which one they got rather than inferring it.
func Describe(localDir string, s3 S3Config) string {
	switch {
	case strings.TrimSpace(s3.Bucket) != "":
		prefix := ""
		if s3.Prefix != "" {
			prefix = "/" + strings.Trim(s3.Prefix, "/")
		}
		return fmt.Sprintf("s3://%s%s at %s", s3.Bucket, prefix, s3.Endpoint)
	case strings.TrimSpace(localDir) != "":
		return "local directory " + localDir
	default:
		return "none"
	}
}
