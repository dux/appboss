package pg

import (
	"context"
	"os"
	"strings"

	"dboss/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
)

// Uploader is the S3 seam: the minio-backed store in production, a fake in tests.
type Uploader interface {
	Configured() bool
	Put(ctx context.Context, key, path string, size int64) error
	Get(ctx context.Context, key, path string) error
	Remove(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// s3Store stores dump objects in one bucket. The key prefix comes from config and is applied by
// the caller, so List already receives the full prefix.
type s3Store struct {
	client *minio.Client
	bucket string
	sse    encrypt.ServerSide
}

// newS3 builds the object store, or returns nil when the block is not fully configured.
func newS3(cfg config.S3) (*s3Store, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	endpoint, secure := parseEndpoint(cfg.Endpoint)
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	store := &s3Store{client: client, bucket: cfg.Bucket}
	if strings.EqualFold(cfg.SSE, "AES256") {
		store.sse = encrypt.NewSSE()
	}
	return store, nil
}

// parseEndpoint strips the scheme minio does not accept and returns whether to use TLS.
func parseEndpoint(endpoint string) (string, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		return strings.TrimSuffix(rest, "/"), true
	}
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		return strings.TrimSuffix(rest, "/"), false
	}
	return strings.TrimSuffix(endpoint, "/"), true
}

func (s *s3Store) Configured() bool { return s != nil }

func (s *s3Store) Put(ctx context.Context, key, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = s.client.PutObject(ctx, s.bucket, key, file, size, minio.PutObjectOptions{
		ContentType:          "application/octet-stream",
		ServerSideEncryption: s.sse,
	})
	return err
}

func (s *s3Store) Get(ctx context.Context, key, path string) error {
	return s.client.FGetObject(ctx, s.bucket, key, path, minio.GetObjectOptions{})
}

func (s *s3Store) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if object.Err != nil {
			return keys, object.Err
		}
		keys = append(keys, object.Key)
	}
	return keys, nil
}
