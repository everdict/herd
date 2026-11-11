package s3_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

func listStore(t *testing.T, srv *s3test.Server, prefix string) *s3.Store {
	t.Helper()
	ts := s3test.Start(t, srv)
	s, err := s3.New(s3.Config{Endpoint: ts.URL, Bucket: "herd", Prefix: prefix, AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestListReadsEveryPage (A-58): a listing longer than one answer is read to its end, through continuation tokens
// that carry characters the signer must encode, and holds only the store's own prefix, with the prefix taken off.
func TestListReadsEveryPage(t *testing.T) {
	srv := s3test.New("us-east-1", creds)
	srv.SetListPage(2)
	s := listStore(t, srv, "snaps")
	for i := range 5 {
		if _, err := s.Put(t.Context(), fmt.Sprintf("sp/actors/a/u/%d", i), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	srv.Plant("/herd/other/sp/actors/a/u/9", []byte("not ours"), "sha256:00", "", 0)
	srv.Plant("/herd/snapsx/sp/1", []byte("not ours either"), "sha256:00", "", 0)
	before := srv.Lists()
	got, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range got {
		keys = append(keys, o.Key)
	}
	if want := "sp/actors/a/u/0 sp/actors/a/u/1 sp/actors/a/u/2 sp/actors/a/u/3 sp/actors/a/u/4"; strings.Join(keys, " ") != want {
		t.Errorf("List = %v, want %s", keys, want)
	}
	if n := srv.Lists() - before; n != 3 {
		t.Errorf("List made %d list calls for 5 keys in pages of 2, want 3", n)
	}
}

// TestAbortUploadsTakesOnlyStaleOnes (A-58): an upload begun before the cutoff and never finished is aborted; one
// begun after it (a Put that may still be sending parts) is left, and so are another prefix's.
func TestAbortUploadsTakesOnlyStaleOnes(t *testing.T) {
	srv := s3test.New("us-east-1", creds)
	srv.SetListPage(1)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	srv.SetClock(func() time.Time { return now })
	s := listStore(t, srv, "snaps")
	srv.Begin("/herd/snaps/sp/actors/a/u/1")
	srv.Begin("/herd/snaps/sp/tags/t/u/1")
	srv.Begin("/herd/other/x")
	now = now.Add(30 * time.Hour)
	srv.Begin("/herd/snaps/sp/actors/b/u/1")
	n, err := s.AbortUploads(t.Context(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || srv.Uploads() != 2 {
		t.Errorf("AbortUploads aborted %d, %d uploads left; want 2 aborted (the stale ones under snaps/), 2 left", n, srv.Uploads())
	}
	if n, err := s.AbortUploads(t.Context(), now.Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("a second AbortUploads = %d, %v; want 0", n, err)
	}
}
