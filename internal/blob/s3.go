package blob

import (
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

// S3Config 描述 S3 兼容存储。Endpoint 形如 https://minio.example.com:9000；
// PathStyle 为 true 时使用 endpoint/bucket/key（MinIO 默认），否则 bucket.endpoint/key。
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
	Prefix    string // 对象 key 前缀
}

// S3 是基于 SigV4 签名的最小 S3 客户端，只实现 PUT / GET / DELETE Object。
type S3 struct {
	cfg  S3Config
	base *url.URL
	http *http.Client
	now  func() time.Time
}

func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", cfg.Endpoint)
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("S3 bucket, access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3{cfg: cfg, base: u, http: &http.Client{Timeout: 30 * time.Minute}, now: time.Now}, nil
}

func (s *S3) objectURL(key string) *url.URL {
	u := *s.base
	key = s.cfg.Prefix + key
	if s.cfg.PathStyle {
		u.Path = "/" + s.cfg.Bucket + "/" + key
	} else {
		u.Host = s.cfg.Bucket + "." + u.Host
		u.Path = "/" + key
	}
	return &u
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !validKey(key) {
		return fmt.Errorf("invalid blob key")
	}
	if size < 0 {
		// S3 PUT 需要 Content-Length，长度未知时先落盘。
		f, err := os.CreateTemp("", "flow-blob-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if size, err = io.Copy(f, r); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		r = f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key).String(), r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	_, err = s.do(req, "UNSIGNED-PAYLOAD")
	return err
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key).String(), nil)
	if err != nil {
		return nil, err
	}
	return s.do(req, emptySHA256)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.objectURL(key).String(), nil)
	if err != nil {
		return err
	}
	body, err := s.do(req, emptySHA256)
	if err == nil {
		body.Close()
	}
	if err == ErrNotFound {
		return nil
	}
	return err
}

func (s *S3) do(req *http.Request, payloadHash string) (io.ReadCloser, error) {
	signV4(req, s.cfg.AccessKey, s.cfg.SecretKey, s.cfg.Region, payloadHash, s.now())
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signV4 按 AWS Signature Version 4 为请求签名，签入 Host 与所有已设置的请求头。
func signV4(req *http.Request, accessKey, secretKey, region, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method, uriEncode(req.URL.Path, false), canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, payloadHash,
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	for _, part := range []string{region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signed, sig))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode 按 SigV4 规则编码：仅保留 A-Z a-z 0-9 - _ . ~，路径中保留 /。
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
