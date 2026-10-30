package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"
)

// TestSignAWSVectors: the signer reproduces AWS's published SigV4 test suite (aws-sig-v4-test-suite get-vanilla and
// get-vanilla-query-order-key-case), so the fake endpoint, which re-signs with the same code, is not the only judge.
func TestSignAWSVectors(t *testing.T) {
	c := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, v := range []struct{ url, want string }{
		{"https://example.amazonaws.com/", "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"https://example.amazonaws.com/?Param2=value2&Param1=value1", "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
	} {
		req, err := http.NewRequest(http.MethodGet, v.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		Sign(req, c, "us-east-1", "service", EmptySHA256, at)
		want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + v.want
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", v.url, got, want)
		}
	}
}

func digestHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
