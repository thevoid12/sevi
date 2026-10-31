package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// R2 is Cloudflare R2 through its S3 API. endpoint is the account S3 host from
// the Cloudflare dashboard, <account-id>.r2.cloudflarestorage.com (a leading
// https:// is accepted). Region is "auto": required by the signer, ignored by R2.
type R2 struct {
	client *minio.Client
	bucket string
}

func NewR2(endpoint, bucket, accessKeyID, secretAccessKey string) (*R2, error) {
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	for name, v := range map[string]string{"R2_ENDPOINT": endpoint, "R2_BUCKET": bucket, "R2_ACCESS_KEY_ID": accessKeyID, "R2_SECRET_ACCESS_KEY": secretAccessKey} {
		if v == "" {
			return nil, fmt.Errorf("%s is required for the r2 object store (set it in the secrets)", name)
		}
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: true,
		Region: "auto",
	})
	if err != nil {
		return nil, fmt.Errorf("r2 client: %w", err)
	}
	return &R2{client: c, bucket: bucket}, nil
}

func (*R2) Kind() string { return "r2" }

func (r *R2) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := r.client.PutObject(ctx, r.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (r *R2) Get(ctx context.Context, key string) ([]byte, error) {
	o, err := r.client.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	b, err := io.ReadAll(o)
	if isNotFound(err) {
		return nil, ErrNotFound
	}
	return b, err
}

func (r *R2) Exists(ctx context.Context, key string) (bool, error) {
	_, err := r.client.StatObject(ctx, r.bucket, key, minio.StatObjectOptions{})
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (r *R2) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := r.client.PresignedGetObject(ctx, r.bucket, key, ttl, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func isNotFound(err error) bool {
	var e minio.ErrorResponse
	return errors.As(err, &e) && (e.Code == "NoSuchKey" || e.StatusCode == 404)
}
