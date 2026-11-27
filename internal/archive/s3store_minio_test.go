//go:build integration

// The S3 driver against a real S3 server.
//
// s3store_test.go exercises it against an httptest fake, which proves the
// code's own logic and nothing about S3. Two things it cannot tell us:
// whether the requests this client builds are ones a real implementation
// accepts, and whether the behaviours the driver relies on are actually S3's
// behaviours rather than the fake's.
//
// The second is the one that matters. isS3NotFound exists because AWS answers
// 204 to a delete whether or not the object was there, so "already gone" has
// to be read from a Get rather than a Delete — and that was a claim about a
// service nobody here had talked to. The fake was written to match the
// claim, which makes it evidence of the claim only if the claim was right.
//
// MinIO is S3-compatible and runs in a container, so this is a real server
// with real signing, real error codes and real multipart behaviour. It is
// not AWS: the remaining gap is AWS-specific behaviour, and it is a much
// smaller gap than a fake.
//
// Skipped unless Docker is present, because a test that cannot run must not
// be a test that fails.
package archive_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
)

const (
	minioImage  = "minio/minio:RELEASE.2024-05-01T01-11-10Z"
	minioUser   = "minioadmin"
	minioPass   = "minioadmin"
	minioPort   = "19100"
	minioName   = "isp-archive-minio-test"
	minioBucket = "isp-archive-test"
)

// One container for the whole package.
//
// Each test used to start its own, which worked and cost about four seconds
// each: nine restarts for a suite whose actual work takes milliseconds. The
// container comes up on first use and is removed by TestMain.
var (
	minioOnce     sync.Once
	minioEndpoint string
	minioWhyNot   string
)

// TestMain removes the container after the package's tests, including when
// one panics. A leftover container fails the next run's port bind, which
// presents as "docker unavailable" and skips the whole suite silently.
func TestMain(m *testing.M) {
	code := m.Run()
	if minioEndpoint != "" {
		_ = exec.Command("docker", "rm", "-f", minioName).Run()
	}
	os.Exit(code)
}

// startMinIO brings the container up on first use and returns the endpoint,
// or skips with the reason it could not.
func startMinIO(t *testing.T) string {
	t.Helper()
	minioOnce.Do(bringUpMinIO)
	if minioWhyNot != "" {
		t.Skip(minioWhyNot)
	}
	return minioEndpoint
}

func bringUpMinIO() {
	if _, err := exec.LookPath("docker"); err != nil {
		minioWhyNot = "docker is not on PATH; cannot reach a real S3 server"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "version",
		"--format", "{{.Server.Version}}").Run(); err != nil {
		minioWhyNot = "the docker daemon is not answering; cannot reach a real S3 server"
		return
	}

	// Any leftover from an interrupted run, which would otherwise fail the
	// port bind and look like an unavailable daemon.
	_ = exec.Command("docker", "rm", "-f", minioName).Run()

	out, err := exec.Command("docker", "run", "-d", "--name", minioName,
		"-p", minioPort+":9000",
		"-e", "MINIO_ROOT_USER="+minioUser,
		"-e", "MINIO_ROOT_PASSWORD="+minioPass,
		minioImage, "server", "/data").CombinedOutput()
	if err != nil {
		minioWhyNot = fmt.Sprintf("could not start MinIO: %v - %s",
			err, strings.TrimSpace(string(out)))
		return
	}

	// mc ships inside the image, so readiness and bucket creation need no
	// second tool on the host.
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		if exec.Command("docker", "exec", minioName, "mc", "alias", "set", "local",
			"http://127.0.0.1:9000", minioUser, minioPass).Run() != nil {
			continue
		}
		if out, err := exec.Command("docker", "exec", minioName,
			"mc", "mb", "--ignore-existing", "local/"+minioBucket).CombinedOutput(); err != nil {
			minioWhyNot = fmt.Sprintf("could not create the bucket: %v - %s",
				err, strings.TrimSpace(string(out)))
			return
		}
		minioEndpoint = "127.0.0.1:" + minioPort
		return
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "20", minioName).CombinedOutput()
	minioWhyNot = "MinIO never became ready: " + strings.TrimSpace(string(logs))
}

func minioStore(t *testing.T, prefix string) *archive.S3Store {
	t.Helper()
	endpoint := startMinIO(t)
	s, err := archive.NewS3Store(archive.S3Config{
		Endpoint:        endpoint,
		Bucket:          minioBucket,
		Prefix:          prefix,
		AccessKeyID:     minioUser,
		SecretAccessKey: minioPass,
		// Plain HTTP against a container on loopback. This also exercises the
		// streaming-signature path, which is the one a real deployment behind
		// a TLS-terminating proxy does not use — so it is the less-travelled
		// of the two and worth being the one under test.
		UseSSL: false,
	})
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	return s
}

// A document written to a real bucket comes back byte for byte, with the
// size and checksum measured over what was actually written.
func TestFR_NET_008_ADocumentSurvivesARoundTripThroughRealObjectStorage(t *testing.T) {
	s := minioStore(t, "invoices")
	ctx := context.Background()

	// Not a tidy string: a PDF is binary, and a driver that works only for
	// text is a driver that has not been tested.
	body := make([]byte, 0, 4096)
	for i := 0; i < 4096; i++ {
		body = append(body, byte(i%251))
	}

	res, err := s.Put(ctx, "2026/11/INV-000042.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", res.SizeBytes, len(body))
	}
	if res.ChecksumSHA256 == "" {
		t.Error("no checksum was returned; an archive that cannot prove a document is " +
			"unchanged is not an archive")
	}
	if !strings.HasPrefix(res.URL, "s3://"+minioBucket+"/") {
		t.Errorf("url = %q", res.URL)
	}
	// The prefix is part of the key, so two deployments can share a bucket.
	if !strings.Contains(res.URL, "invoices/") {
		t.Errorf("url %q does not carry the configured prefix", res.URL)
	}

	rc, err := s.Get(ctx, res.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("%d bytes came back, want %d, and they differ", len(got), len(body))
	}
}

// The behaviour isS3NotFound was written for, checked against a real server
// rather than against a fake built to match the assumption.
//
// Deleting something already gone must not be an error — the purge scanner
// retries, and a backend that reported a second delete as a failure would
// make a retried purge look like a broken one forever.
func TestFR_NET_008_DeletingSomethingAlreadyGoneIsNotAnError(t *testing.T) {
	s := minioStore(t, "")
	ctx := context.Background()

	res, err := s.Put(ctx, uniqueKey("purge-me.pdf"), strings.NewReader("a document"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.Delete(ctx, res.URL); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// The second one is the test.
	if err := s.Delete(ctx, res.URL); err != nil {
		t.Errorf("deleting an object that is already gone reported an error: %v\n"+
			"The purge scanner retries, so this would make every retried purge look "+
			"like a permanent failure.", err)
	}
	// And a key that never existed.
	if err := s.Delete(ctx, "s3://"+minioBucket+"/never-existed.pdf"); err != nil {
		t.Errorf("deleting a key that never existed reported an error: %v", err)
	}

	// What this does and does not establish, measured rather than assumed.
	//
	// Stubbing isS3NotFound to return false leaves this test passing and
	// fails only the fake's 404 case — so MinIO, like AWS, answers 2xx to a
	// delete of something absent and never reaches that branch. The
	// behaviour the driver was designed around is confirmed against a real
	// server; the 404 tolerance remains covered only by the fake, because no
	// real server available here produces it.
	//
	// That is worth stating plainly: a later reader should not conclude this
	// file proved both branches.
}

// Getting something that is not there is an error, and a distinguishable one.
// This is the other half: Delete is silent about absence, so Get is the only
// place absence can be learned.
func TestFR_NET_008_GettingSomethingAbsentIsAnError(t *testing.T) {
	s := minioStore(t, "")
	ctx := context.Background()

	rc, err := s.Get(ctx, "s3://"+minioBucket+"/definitely-not-here.pdf")
	if err == nil {
		rc.Close()
		t.Fatal("reading an object that does not exist succeeded")
	}
	// Not an empty reader pretending to be a document.
	if rc != nil {
		t.Error("a reader was returned alongside the error")
	}
}

// A URL belonging to a different bucket is refused rather than acted on. One
// deployment must not be able to read or delete another's objects by handing
// over a URL.
func TestFR_NET_008_AURLForAnotherBucketIsRefused(t *testing.T) {
	s := minioStore(t, "")
	ctx := context.Background()

	for _, url := range []string{
		"s3://someone-elses-bucket/invoice.pdf",
		"s3://" + minioBucket + "-other/invoice.pdf",
		"https://" + minioBucket + ".s3.amazonaws.com/invoice.pdf",
		"file:///etc/passwd",
		"not-a-url",
		"",
	} {
		if _, err := s.Get(ctx, url); err == nil {
			t.Errorf("Get(%q) was accepted", url)
		}
		if err := s.Delete(ctx, url); err == nil {
			t.Errorf("Delete(%q) was accepted", url)
		}
	}
}

// Put is safe to call twice with the same key, because a retry after a
// network failure is normal.
func TestFR_NET_008_PuttingTheSameKeyTwiceIsSafe(t *testing.T) {
	s := minioStore(t, "")
	ctx := context.Background()

	key := uniqueKey("retried.pdf")
	first, err := s.Put(ctx, key, strings.NewReader("the first attempt"))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	second, err := s.Put(ctx, key, strings.NewReader("the second attempt"))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if first.URL != second.URL {
		t.Errorf("the same key produced two URLs: %q and %q", first.URL, second.URL)
	}

	rc, err := s.Get(ctx, second.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "the second attempt" {
		t.Errorf("after a retry the object reads %q; a half-overwritten object would be "+
			"worse than either version", string(got))
	}
}

// An empty document is refused before it reaches the bucket. A zero-byte
// invoice that verifies against its own checksum is a document that is gone
// and does not look it.
func TestFR_NET_008_AnEmptyDocumentIsRefused(t *testing.T) {
	s := minioStore(t, "")
	if _, err := s.Put(context.Background(), "empty.pdf", strings.NewReader("")); err == nil {
		t.Error("an empty document was archived")
	}
}

// The size cap is enforced against a real upload, not only in the length
// check: the point is that nothing partial is left in the bucket.
func TestFR_NET_008_AnOversizedDocumentIsRefusedAndLeavesNothingBehind(t *testing.T) {
	endpoint := startMinIO(t)
	s, err := archive.NewS3Store(archive.S3Config{
		Endpoint: endpoint, Bucket: minioBucket,
		AccessKeyID: minioUser, SecretAccessKey: minioPass, UseSSL: false,
		MaxObjectBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	big := strings.Repeat("x", 4096)
	if _, err := s.Put(ctx, "too-big.pdf", strings.NewReader(big)); err == nil {
		t.Fatal("a document over the cap was archived")
	}
	// Nothing visible under that key afterwards.
	if rc, err := s.Get(ctx, "s3://"+minioBucket+"/too-big.pdf"); err == nil {
		rc.Close()
		t.Error("a refused upload left an object behind")
	}

	// And one just under the cap goes through, so the cap is a cap and not a
	// blanket refusal.
	if _, err := s.Put(ctx, "just-right.pdf", strings.NewReader(strings.Repeat("x", 1000))); err != nil {
		t.Errorf("a document inside the cap was refused: %v", err)
	}
}

// Credentials that do not work surface as an error on the first Put rather
// than at construction — a daemon that refused to boot because object storage
// was briefly unreachable would trade a degraded archive for no service.
func TestFR_NET_008_BadCredentialsFailOnUseNotOnConstruction(t *testing.T) {
	endpoint := startMinIO(t)

	s, err := archive.NewS3Store(archive.S3Config{
		Endpoint: endpoint, Bucket: minioBucket,
		AccessKeyID: "wrong", SecretAccessKey: "alsowrong", UseSSL: false,
	})
	if err != nil {
		t.Fatalf("construction should not probe the bucket: %v", err)
	}
	_, err = s.Put(context.Background(), "nope.pdf", strings.NewReader("x"))
	if err == nil {
		t.Fatal("a Put with bad credentials succeeded")
	}
	if !strings.Contains(err.Error(), minioBucket) {
		t.Errorf("the error does not say which bucket failed: %v", err)
	}
}

// A bucket that does not exist is reported as such, and the message names it.
func TestFR_NET_008_AMissingBucketIsReportedWithItsName(t *testing.T) {
	endpoint := startMinIO(t)

	s, err := archive.NewS3Store(archive.S3Config{
		Endpoint: endpoint, Bucket: "no-such-bucket-here",
		AccessKeyID: minioUser, SecretAccessKey: minioPass, UseSSL: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), "x.pdf", strings.NewReader("x"))
	if err == nil {
		t.Fatal("a Put into a nonexistent bucket succeeded")
	}
	if !strings.Contains(err.Error(), "no-such-bucket-here") {
		t.Errorf("the error does not name the bucket: %v", err)
	}
}

// NewStore prefers S3 when a bucket is configured, and says which backend it
// chose — an operator who thinks documents are going off-machine when they
// are going to a local directory has no way to tell from the behaviour.
func TestFR_NET_008_NewStorePrefersS3AndDescribesItsChoice(t *testing.T) {
	endpoint := startMinIO(t)
	cfg := archive.S3Config{
		Endpoint: endpoint, Bucket: minioBucket,
		AccessKeyID: minioUser, SecretAccessKey: minioPass, UseSSL: false,
	}

	s, err := archive.NewStore(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if s.Backend() != archive.BackendS3 {
		t.Errorf("backend = %q with a bucket configured, want %q", s.Backend(), archive.BackendS3)
	}
	if d := archive.Describe(t.TempDir(), cfg); !strings.Contains(d, minioBucket) {
		t.Errorf("Describe does not name the bucket: %q", d)
	}

	// And it really is the S3 one: a round trip lands in the bucket.
	res, err := s.Put(context.Background(), "chosen.pdf", strings.NewReader("off-machine"))
	if err != nil {
		t.Fatalf("put through NewStore: %v", err)
	}
	if !strings.HasPrefix(res.URL, "s3://") {
		t.Errorf("NewStore returned something that writes to %q", res.URL)
	}
}

// The error type the driver keys off is the real one. If this ever stops
// matching, Delete starts reporting absence as failure again.
func TestFR_NET_008_AbsenceIsDistinguishableFromFailure(t *testing.T) {
	s := minioStore(t, "")
	ctx := context.Background()

	// Absent: Get fails, Delete does not.
	absent := "s3://" + minioBucket + "/absent-" + fmt.Sprint(time.Now().UnixNano()) + ".pdf"
	getErr := func() error {
		rc, err := s.Get(ctx, absent)
		if rc != nil {
			rc.Close()
		}
		return err
	}()
	delErr := s.Delete(ctx, absent)

	if getErr == nil {
		t.Error("Get on an absent object succeeded")
	}
	if delErr != nil {
		t.Errorf("Delete on an absent object failed: %v", delErr)
	}
	// The two must not be the same error, or the caller cannot tell them
	// apart.
	if getErr != nil && delErr != nil && errors.Is(getErr, delErr) {
		t.Error("absence and failure are the same error")
	}
}

// uniqueKey keeps tests from colliding now that they share one bucket.
func uniqueKey(name string) string {
	return fmt.Sprintf("t-%d/%s", time.Now().UnixNano(), name)
}
