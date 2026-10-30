package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EmptySHA256 is the hex SHA-256 of no bytes, the payload hash of a request without a body.
const EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials sign requests.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken, if set, is sent as X-Amz-Security-Token (temporary credentials).
	SessionToken string
}

// Sign signs req with AWS Signature Version 4 for service in region at t: it sets X-Amz-Date (and
// X-Amz-Security-Token) and Authorization. The signed headers are host and every X-Amz-* header req carries;
// payloadHash is the hex SHA-256 of the body the request sends (S3 checks it against the body it receives).
func Sign(req *http.Request, c Credentials, region, service, payloadHash string, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	signed, canonHeaders := canonicalHeaders(req)
	creq := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		canonicalQuery(req),
		canonHeaders,
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, sts))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func canonicalHeaders(req *http.Request) (signed, canon string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	vals := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			trimmed := make([]string, len(v))
			for i, s := range v {
				trimmed[i] = strings.Join(strings.Fields(s), " ")
			}
			vals[lk] = strings.Join(trimmed, ",")
		}
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + ":" + vals[k] + "\n")
	}
	return strings.Join(names, ";"), b.String()
}

func canonicalQuery(req *http.Request) string {
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's URI encoding: every byte but the unreserved ones is %XX; '/' too when encodeSlash.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
