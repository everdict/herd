package s3_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"

	"github.com/everdict/herd/snapstore"
)

// TestProveReadsNothing (DEFAUL-1610): Prove of an object Put wrote answers from a HEAD, without reading the object:
// the endpoint's SHA-256 of one PUT is the digest, and its composite checksum of a multipart object the one Put
// recorded with it. Its counterexample is the check before DEFAUL-1610, Verify, which herd-api ran after every
// suspend and which read the whole object (2m43s live).
func TestProveReadsNothing(t *testing.T) {
	for _, c := range copyShapes {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t, c.size, c.partSize)
			served, _ := f.counts()
			if got, err := f.s.Prove(t.Context(), "src", f.src.Digest); err != nil || got != f.src {
				t.Fatalf("Prove = %+v, %v; want %+v", got, err, f.src)
			}
			if s, _ := f.counts(); s != served {
				t.Errorf("Prove read %d bytes of the object; the endpoint's checksum proves it", s-served)
			}
		})
	}
}

// TestProveRefusesWhatTheChecksumDenies: an object whose bytes are not its herd digest's (a writer that lied, an
// endpoint that broke) is refused by its checksum, with no byte read: a full-object one that is not the digest, a
// composite one that is not the one recorded. From an endpoint that keeps no checksum, and for a composite checksum
// nothing was recorded for (an object written before DEFAUL-1610), Prove reads the object, and refuses it by its
// bytes.
func TestProveRefusesWhatTheChecksumDenies(t *testing.T) {
	data, other := []byte("the bytes the endpoint holds"), []byte("the bytes the digest is of!")
	sum, otherSum := sha256.Sum256(data), sha256.Sum256(other)
	digest, otherDigest := "sha256:"+hex.EncodeToString(sum[:]), "sha256:"+hex.EncodeToString(otherSum[:])
	compositeOf := func(b []byte, part int) string {
		var sums []byte
		n := 0
		for off := 0; off < len(b); off += part {
			s := sha256.Sum256(b[off:min(off+part, len(b))])
			sums, n = append(sums, s[:]...), n+1
		}
		c := sha256.Sum256(sums)
		return base64.StdEncoding.EncodeToString(c[:]) + "-" + strconv.Itoa(n)
	}
	for _, c := range []struct {
		name       string
		recorded   string
		part       int
		noChecksum bool
		read       bool
	}{
		{"full checksum", "", 0, false, false},
		{"composite checksum", compositeOf(other, 8), 8, false, false},
		{"no checksum", "", 0, true, true},
		{"composite, nothing recorded", "", 8, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t, 16, 0)
			f.srv.SetNoChecksum(c.noChecksum)
			f.srv.Plant("/herd/lied", data, otherDigest, c.recorded, c.part)
			served, _ := f.counts()
			if got, err := f.s.Prove(t.Context(), "lied", otherDigest); !errors.Is(err, snapstore.ErrDigestMismatch) {
				t.Errorf("Prove of bytes that are not the digest's = %+v, %v; want ErrDigestMismatch", got, err)
			}
			if s, _ := f.counts(); c.read != (s != served) {
				t.Errorf("Prove read %d bytes; want a read: %v", s-served, c.read)
			}
			// The same bytes under their own digest are proved, with the recorded checksum of their parts.
			rec := ""
			if c.recorded != "" {
				rec = compositeOf(data, c.part)
			}
			f.srv.Plant("/herd/true", data, digest, rec, c.part)
			if got, err := f.s.Prove(t.Context(), "true", digest); err != nil || got.Digest != digest {
				t.Errorf("Prove of the digest's bytes = %+v, %v", got, err)
			}
		})
	}
}
