package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 is a small path-style S3 client for MinIO and AWS-compatible endpoints.
// It deliberately supports only the operations the exporter needs.
type S3 struct {
	Endpoint, Region, AccessKey, SecretKey string
	Client                                 *http.Client
}

func New(endpoint, region, access, secret string) *S3 {
	return &S3{Endpoint: strings.TrimRight(endpoint, "/"), Region: region, AccessKey: access, SecretKey: secret, Client: &http.Client{Timeout: 2 * time.Minute}}
}
func (s *S3) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return s.GetVersion(ctx, bucket, key, "")
}
func (s *S3) GetVersion(ctx context.Context, bucket, key, version string) (io.ReadCloser, error) {
	q := url.Values{}
	if version != "" {
		q.Set("versionId", version)
	}
	resp, e := s.requestQuery(ctx, http.MethodGet, bucket, key, nil, "", q)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("S3 GET returned %s", resp.Status)
	}
	return resp.Body, nil
}
func (s *S3) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) error {
	resp, e := s.request(ctx, http.MethodPut, bucket, key, body, contentType, size)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("S3 PUT returned %s", resp.Status)
	}
	return nil
}
func (s *S3) Delete(ctx context.Context, bucket, key string) error {
	resp, e := s.request(ctx, http.MethodDelete, bucket, key, nil, "")
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("S3 DELETE returned %s", resp.Status)
	}
	return nil
}
func (s *S3) PresignGet(bucket, key string, expiry time.Duration) (string, error) {
	return s.PresignGetAt(s.Endpoint, bucket, key, expiry)
}
func (s *S3) PresignGetAt(endpoint, bucket, key string, expiry time.Duration) (string, error) {
	if expiry <= 0 || expiry > 7*24*time.Hour {
		return "", fmt.Errorf("presign expiry out of range")
	}
	u, e := parseObjectURL(endpoint, bucket, key)
	if e != nil {
		return "", e
	}
	now := time.Now().UTC()
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	scope := date + "/" + s.Region + "/s3/aws4_request"
	cred := s.AccessKey + "/" + scope
	params := url.Values{"X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-Credential": {cred}, "X-Amz-Date": {stamp}, "X-Amz-Expires": {fmt.Sprint(int(expiry.Seconds()))}, "X-Amz-SignedHeaders": {"host"}}
	u.RawQuery = canonicalQuery(params)
	canonical := "GET\n" + canonicalURI(u) + "\n" + u.RawQuery + "\nhost:" + u.Host + "\n\nhost\nUNSIGNED-PAYLOAD"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hash([]byte(canonical))
	signature := hex.EncodeToString(hmacBytes(signingKey(s.SecretKey, date, s.Region), []byte(toSign)))
	params.Set("X-Amz-Signature", signature)
	u.RawQuery = canonicalQuery(params)
	return u.String(), nil
}
func (s *S3) request(ctx context.Context, method, bucket, key string, body io.Reader, contentType string, size ...int64) (*http.Response, error) {
	return s.requestQuery(ctx, method, bucket, key, body, contentType, nil, size...)
}
func (s *S3) requestQuery(ctx context.Context, method, bucket, key string, body io.Reader, contentType string, query url.Values, size ...int64) (*http.Response, error) {
	u, e := url.Parse(s.objectURL(bucket, key))
	if e != nil {
		return nil, e
	}
	if query != nil {
		u.RawQuery = canonicalQuery(query)
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	payload := "UNSIGNED-PAYLOAD"
	canonicalHeaders := "host:" + u.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + stamp + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	if contentType != "" {
		canonicalHeaders = "content-type:" + contentType + "\n" + canonicalHeaders
		signedHeaders = "content-type;" + signedHeaders
	}
	var canonical strings.Builder
	canonical.WriteString(method + "\n" + canonicalURI(u) + "\n" + u.RawQuery + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payload)
	scope := date + "/" + s.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hash([]byte(canonical.String()))
	signature := hex.EncodeToString(hmacBytes(signingKey(s.SecretKey, date, s.Region), []byte(toSign)))
	req, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if len(size) > 0 {
		req.ContentLength = size[0]
	}
	return s.Client.Do(req)
}
func (s *S3) objectURL(bucket, key string) string {
	u, e := parseObjectURL(s.Endpoint, bucket, key)
	if e != nil {
		return ""
	}
	return u.String()
}
func parseObjectURL(endpoint, bucket, key string) (*url.URL, error) {
	u, e := url.Parse(strings.TrimRight(endpoint, "/"))
	if e != nil {
		return nil, e
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + bucket + "/" + strings.TrimLeft(key, "/")
	u.RawPath = ""
	return u, nil
}
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}
func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		vals := v[k]
		sort.Strings(vals)
		for _, x := range vals {
			parts = append(parts, awsEncode(k)+"="+awsEncode(x))
		}
	}
	return strings.Join(parts, "&")
}
func awsEncode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func hash(b []byte) string      { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func hmacBytes(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
func signingKey(secret, date, region string) []byte {
	kDate := hmacBytes([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacBytes(kDate, []byte(region))
	kService := hmacBytes(kRegion, []byte("s3"))
	return hmacBytes(kService, []byte("aws4_request"))
}
