package archive

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The S3 store is tested against a server that speaks S3's HTTP API rather
// than against a mocked client.
//
// Mocking minio.Client would assert that this code calls the methods this code
// calls, which is worth nothing. What is actually in question is whether the
// bytes, the key and the verbs that go over the wire are right, and whether
// the responses a real bucket returns are handled — a 404 on delete being the
// one that decides whether the purge scanner retries for ever. Same approach
// as internal/nastraffic/snmptest, which runs a real agent on a real socket.
//
// FR-NET-008. Needs no credentials and no bucket, which is what let this be
// built while the real bucket is still an open question.

// fakeS3 implements the subset of S3 this store uses: PUT, GET and DELETE of
// an object, plus the HEAD that minio issues to locate a bucket's region.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	// putCount per key, so a retry can be shown to overwrite rather than
	// duplicate.
	putCount map[string]int
	// failPut makes the next PUT fail, for the error path.
	failPut bool
	// notFoundOnDelete makes DELETE answer 404 for an absent key. AWS answers
	// 204 whether or not the object was there, but not every S3-compatible
	// backend does, and the purge scanner must survive both.
	notFoundOnDelete bool
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, putCount: map[string]int{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// minio asks for the bucket's location before its first operation.
	if r.Method == http.MethodGet && r.URL.Query().Has("location") {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint>us-east-1</LocationConstraint>`)
		return
	}

	// Path style: /bucket/key...
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	_ = bucket

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodHead:
		if _, ok := f.objects[key]; !ok {
			f.notFound(w, key)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(f.objects[key])))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+etagOf(f.objects[key])+`"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodPut:
		if f.failPut {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>InternalError</Code></Error>`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.objects[key] = body
		f.putCount[key]++
		w.Header().Set("ETag", `"`+etagOf(body)+`"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			f.notFound(w, key)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+etagOf(body)+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case http.MethodDelete:
		if _, ok := f.objects[key]; !ok && f.notFoundOnDelete {
			f.notFound(w, key)
			return
		}
		// AWS answers 204 whether or not the object was there.
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// etagOf is S3's MD5-shaped entity tag. Nothing under test reads it, but a
// client may, and an absent ETag is not something a real bucket ever returns.
func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func (f *fakeS3) notFound(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code>`+
		`<Message>The specified key does not exist.</Message><Key>%s</Key></Error>`, key)
}

func newTestS3Store(t *testing.T, prefix string) (*S3Store, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	// TLS, because that is how this runs against a real bucket and because
	// over plain HTTP the client signs the body with a streaming signature
	// that wraps it in aws-chunked framing — a real S3 unwraps it, and a test
	// double that did not would be asserting against the framing rather than
	// the document.
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)

	store, err := NewS3Store(S3Config{
		Endpoint:        strings.TrimPrefix(srv.URL, "https://"),
		Bucket:          "isp-archive",
		Prefix:          prefix,
		AccessKeyID:     "test",
		SecretAccessKey: "testsecret",
		Region:          "us-east-1",
		UseSSL:          true,
		// The server's own client trusts its self-signed certificate.
		Transport: srv.Client().Transport,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store, fake
}

func TestFR_NET_008_S3Store_PutGetDeleteRoundTrip(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	ctx := context.Background()
	body := []byte("%PDF-1.7 a believable invoice")

	res, err := store.Put(ctx, "invoices/2026/11/INV-000001.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "s3://isp-archive/invoices/2026/11/INV-000001.pdf"; res.URL != want {
		t.Errorf("URL = %q, want %q", res.URL, want)
	}
	if res.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d", res.SizeBytes, len(body))
	}
	// Measured over the bytes as written, which is what the archive verifies
	// against later.
	sum := sha256.Sum256(body)
	if res.ChecksumSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("checksum = %s, want %s", res.ChecksumSHA256, hex.EncodeToString(sum[:]))
	}
	if store.Backend() != BackendS3 {
		t.Errorf("Backend() = %q, want %q", store.Backend(), BackendS3)
	}

	// And the bytes really went over the wire.
	if got := string(fake.objects["invoices/2026/11/INV-000001.pdf"]); got != string(body) {
		t.Errorf("stored %q, want %q", got, string(body))
	}

	rc, err := store.Get(ctx, res.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(read, body) {
		t.Errorf("read back %q, want %q", read, body)
	}

	if err := store.Delete(ctx, res.URL); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := fake.objects["invoices/2026/11/INV-000001.pdf"]; ok {
		t.Error("the object survived Delete")
	}
}

// Store's contract: safe to call twice with the same key, because a retry
// after a network failure is normal.
func TestFR_NET_008_S3Store_PutIsSafeToRetry(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	ctx := context.Background()
	body := []byte("the same document, uploaded twice")

	first, err := store.Put(ctx, "kyc/42.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := store.Put(ctx, "kyc/42.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("retry Put: %v", err)
	}
	if first.URL != second.URL || first.ChecksumSHA256 != second.ChecksumSHA256 {
		t.Errorf("a retry should land in the same place with the same checksum: %+v vs %+v", first, second)
	}
	if n := fake.putCount["kyc/42.pdf"]; n != 2 {
		t.Errorf("want 2 uploads, got %d", n)
	}
	if len(fake.objects) != 1 {
		t.Errorf("a retry duplicated the object: %d objects stored", len(fake.objects))
	}
}

// Deleting something already gone must succeed, or the purge scanner retries
// for ever over an object nobody can remove.
func TestFR_NET_008_S3Store_DeletingSomethingAlreadyGoneSucceeds(t *testing.T) {
	// Two backends, because they disagree and the scanner must survive both.
	t.Run("backend answers 204, as AWS does", func(t *testing.T) {
		store, _ := newTestS3Store(t, "")
		if err := store.Delete(context.Background(), "s3://isp-archive/never/existed.pdf"); err != nil {
			t.Errorf("deleting an absent object must not be an error, got %v", err)
		}
	})
	// This is the case that reaches isS3NotFound. Without it the guard is
	// unreachable from the tests, because AWS's own tolerance hides it — a
	// control that removed the guard passed until this subtest existed.
	t.Run("backend answers 404", func(t *testing.T) {
		store, fake := newTestS3Store(t, "")
		fake.notFoundOnDelete = true
		if err := store.Delete(context.Background(), "s3://isp-archive/never/existed.pdf"); err != nil {
			t.Errorf("a 404 on delete must be treated as already gone, or the purge scanner "+
				"retries for ever over an object nobody can remove: %v", err)
		}
	})
}

// storage_url comes back out of the database, so it is re-validated rather
// than trusted — the same reasoning LocalStore applies to a path.
func TestFR_NET_008_S3Store_RefusesAURLOutsideItsOwnBucket(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	ctx := context.Background()
	fake.objects["someone-elses.pdf"] = []byte("not ours")

	for _, tc := range []struct{ name, url string }{
		{"another bucket", "s3://someone-elses-bucket/secrets.pdf"},
		{"not an s3 url", "file:///etc/passwd"},
		{"no object named", "s3://isp-archive/"},
		{"no object named at all", "s3://isp-archive"},
	} {
		t.Run("get "+tc.name, func(t *testing.T) {
			if _, err := store.Get(ctx, tc.url); err == nil {
				t.Errorf("Get(%q) should be refused", tc.url)
			}
		})
		t.Run("delete "+tc.name, func(t *testing.T) {
			if err := store.Delete(ctx, tc.url); err == nil {
				t.Errorf("Delete(%q) should be refused", tc.url)
			}
		})
	}
	// A refused delete must not have removed anything.
	if _, ok := fake.objects["someone-elses.pdf"]; !ok {
		t.Error("a refused delete removed an object anyway")
	}
}

// A prefix lets one bucket hold several deployments. A URL outside it is
// another deployment's object.
func TestFR_NET_008_S3Store_PrefixIsAppliedAndEnforced(t *testing.T) {
	store, fake := newTestS3Store(t, "chennai-north")
	ctx := context.Background()

	res, err := store.Put(ctx, "invoices/INV-1.pdf", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "s3://isp-archive/chennai-north/invoices/INV-1.pdf"; res.URL != want {
		t.Errorf("URL = %q, want %q", res.URL, want)
	}
	if _, ok := fake.objects["chennai-north/invoices/INV-1.pdf"]; !ok {
		t.Errorf("the prefix did not reach the wire: %v", keysOf(fake))
	}
	// Another deployment's key in the same bucket is not ours to read.
	if _, err := store.Get(ctx, "s3://isp-archive/chennai-south/invoices/INV-1.pdf"); err == nil {
		t.Error("a key outside the prefix should be refused")
	}
}

// Same refusal as LocalStore: the schema requires size_bytes > 0, and an empty
// object is a successful-looking record of nothing.
func TestFR_NET_008_S3Store_RefusesAnEmptyDocument(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	if _, err := store.Put(context.Background(), "empty.pdf", bytes.NewReader(nil)); err == nil {
		t.Error("an empty document should be refused")
	}
	if len(fake.objects) != 0 {
		t.Error("an empty document was uploaded anyway")
	}
}

// The cap is explicit so the memory bound is enforced rather than hoped for.
func TestFR_NET_008_S3Store_RefusesSomethingOverTheSizeLimit(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	store.maxBytes = 1024

	// Exactly at the limit is fine.
	if _, err := store.Put(context.Background(), "atlimit.bin",
		bytes.NewReader(bytes.Repeat([]byte("a"), 1024))); err != nil {
		t.Errorf("an object exactly at the limit should be accepted: %v", err)
	}
	// One byte over is refused rather than silently truncated, which is the
	// failure that would produce a corrupt archive that verifies against the
	// wrong checksum.
	if _, err := store.Put(context.Background(), "overlimit.bin",
		bytes.NewReader(bytes.Repeat([]byte("a"), 1025))); err == nil {
		t.Error("an object over the limit should be refused")
	}
	if _, ok := fake.objects["overlimit.bin"]; ok {
		t.Error("the oversized object was uploaded anyway")
	}
}

func TestFR_NET_008_S3Store_ConfigurationErrors(t *testing.T) {
	if _, err := NewS3Store(S3Config{Endpoint: "s3.example.com"}); err == nil {
		t.Error("no bucket should be refused")
	} else if err != ErrS3NotConfigured {
		t.Errorf("no bucket should report ErrS3NotConfigured so a deployment that has not "+
			"enabled archival is distinguishable from a broken one, got %v", err)
	}
	if _, err := NewS3Store(S3Config{Bucket: "isp-archive"}); err == nil {
		t.Error("a bucket with no endpoint should be refused")
	}
}

// A failing upload must not report success, and must leave nothing behind.
func TestFR_NET_008_S3Store_AFailedUploadIsReported(t *testing.T) {
	store, fake := newTestS3Store(t, "")
	fake.failPut = true
	res, err := store.Put(context.Background(), "invoices/INV-9.pdf", bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("a failed upload must be reported")
	}
	if res != nil {
		t.Errorf("a failed upload must not return a result, got %+v", res)
	}
	if len(fake.objects) != 0 {
		t.Error("a failed upload left an object behind")
	}
}

// Get on a missing object fails in Get, not later inside whatever is copying
// the bytes.
func TestFR_NET_008_S3Store_GetReportsAMissingObjectImmediately(t *testing.T) {
	store, _ := newTestS3Store(t, "")
	if _, err := store.Get(context.Background(), "s3://isp-archive/gone.pdf"); err == nil {
		t.Error("Get on a missing object should fail in Get")
	}
}

func keysOf(f *fakeS3) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	return out
}
