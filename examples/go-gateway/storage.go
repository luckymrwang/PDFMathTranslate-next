package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3Config configures an S3-compatible object store (AWS S3, Aliyun OSS via its
// S3-compatible endpoint, MinIO, Cloudflare R2, ...). All values come from env.
type S3Config struct {
	Endpoint  string // e.g. https://oss-cn-hangzhou.aliyuncs.com
	Region    string // e.g. cn-hangzhou (or us-east-1)
	Bucket    string
	AccessKey string // secret
	SecretKey string // secret
	PathStyle bool   // path-style URLs (bucket in path); true for MinIO/most
	URLTTL    time.Duration
	host      string
	scheme    string
}

// LoadS3Config reads object-store settings from the environment. The second
// return value reports whether storage is configured (all required keys set).
func LoadS3Config() (*S3Config, bool) {
	endpoint := os.Getenv("OSS_ENDPOINT")
	bucket := os.Getenv("OSS_BUCKET")
	ak := os.Getenv("OSS_ACCESS_KEY")
	sk := os.Getenv("OSS_SECRET_KEY")
	if endpoint == "" || bucket == "" || ak == "" || sk == "" {
		return nil, false
	}

	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, false
	}

	cfg := &S3Config{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		Region:    getenv("OSS_REGION", "us-east-1"),
		Bucket:    bucket,
		AccessKey: ak,
		SecretKey: sk,
		PathStyle: getenv("OSS_PATH_STYLE", "true") != "false",
		URLTTL:    parseDurationSeconds("OSS_URL_TTL", time.Hour),
		host:      u.Host,
		scheme:    u.Scheme,
	}
	return cfg, true
}

func parseDurationSeconds(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil || secs <= 0 {
		return fallback
	}
	return time.Duration(secs) * time.Second
}

// S3Client performs signed requests against an S3-compatible store.
type S3Client struct {
	cfg  *S3Config
	HTTP *http.Client
}

func NewS3Client(cfg *S3Config) *S3Client {
	return &S3Client{cfg: cfg, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// objectPath builds the canonical URI path for a key (path- or virtual-style).
func (c *S3Client) objectPath(key string) string {
	if c.cfg.PathStyle {
		return "/" + c.cfg.Bucket + "/" + key
	}
	return "/" + key
}

// objectHost returns the request host (adds bucket prefix for virtual-style).
func (c *S3Client) objectHost() string {
	if c.cfg.PathStyle {
		return c.cfg.host
	}
	return c.cfg.Bucket + "." + c.cfg.host
}

// PutObject uploads an object using SigV4 with an unsigned streaming payload.
func (c *S3Client) PutObject(ctx context.Context, key, contentType string, body io.Reader, size int64) error {
	host := c.objectHost()
	canonicalURI := uriEncodePath(c.objectPath(key))
	rawURL := c.cfg.scheme + "://" + host + canonicalURI

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	const payloadHash = "UNSIGNED-PAYLOAD"

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Host", host)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)

	// Canonical headers (sorted): content-type;host;x-amz-content-sha256;x-amz-date
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "content-type:" + contentType + "\n" +
		"host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"

	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		canonicalURI,
		"", // empty query
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	authz := c.authorizationHeader(canonicalRequest, amzDate, dateStamp, signedHeaders)
	req.Header.Set("Authorization", authz)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 put %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// PresignGetURL returns a time-limited GET URL for the object (SigV4 query signing).
func (c *S3Client) PresignGetURL(key string) string {
	host := c.objectHost()
	canonicalURI := uriEncodePath(c.objectPath(key))

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	expires := int(c.cfg.URLTTL.Seconds())

	scope := dateStamp + "/" + c.cfg.Region + "/s3/aws4_request"
	credential := c.cfg.AccessKey + "/" + scope

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", credential)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", expires))
	q.Set("X-Amz-SignedHeaders", "host")
	canonicalQuery := encodeQuerySorted(q)

	canonicalHeaders := "host:" + host + "\n"
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	signature := c.signature(canonicalRequest, amzDate, dateStamp)
	return c.cfg.scheme + "://" + host + canonicalURI + "?" + canonicalQuery +
		"&X-Amz-Signature=" + signature
}

// authorizationHeader builds the SigV4 Authorization header value.
func (c *S3Client) authorizationHeader(canonicalRequest, amzDate, dateStamp, signedHeaders string) string {
	scope := dateStamp + "/" + c.cfg.Region + "/s3/aws4_request"
	signature := c.signature(canonicalRequest, amzDate, dateStamp)
	return "AWS4-HMAC-SHA256 " +
		"Credential=" + c.cfg.AccessKey + "/" + scope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
}

// signature derives the signing key and signs the string-to-sign.
func (c *S3Client) signature(canonicalRequest, amzDate, dateStamp string) string {
	scope := dateStamp + "/" + c.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(c.cfg.Region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uriEncodePath encodes a URI path per SigV4 rules, preserving '/'.
func uriEncodePath(p string) string {
	var b strings.Builder
	for _, seg := range strings.Split(p, "/") {
		b.WriteString(awsEncode(seg))
		b.WriteByte('/')
	}
	s := b.String()
	return strings.TrimSuffix(s, "/")
}

// encodeQuerySorted produces a canonical, sorted, SigV4-encoded query string.
func encodeQuerySorted(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, awsEncode(k)+"="+awsEncode(q.Get(k)))
	}
	return strings.Join(parts, "&")
}

// awsEncode percent-encodes per RFC 3986 (unreserved chars left as-is).
func awsEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if strings.IndexByte(unreserved, ch) >= 0 {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
