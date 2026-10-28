package origin

import (
	"context"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/taktekhq/flatpix/internal/ferr"
	"github.com/taktekhq/flatpix/internal/s3"
)

// S3 serves originals from an S3-compatible bucket.
type S3 struct {
	c      *minio.Client
	bucket string
	prefix string
}

// NewS3 opens a bucket (and an optional key prefix) as an origin.
func NewS3(cfg s3.Config, bucket, prefix string) (*S3, error) {
	c, err := cfg.Client()
	if err != nil {
		return nil, err
	}
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3{c: c, bucket: bucket, prefix: prefix}, nil
}

func (s *S3) objectKey(key string) (string, bool) {
	k, ok := cleanKey(key)
	if !ok {
		return "", false
	}
	return s.prefix + k, true
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	ok, found := s.objectKey(key)
	if !found {
		return Info{}, ferr.New(ferr.ErrNotFound, "image not found")
	}
	st, err := s.c.StatObject(ctx, s.bucket, ok, minio.StatObjectOptions{})
	if err != nil {
		return Info{}, s.err(ctx, err)
	}
	return Info{Key: key, Size: st.Size, ETag: strings.Trim(st.ETag, `"`)}, nil
}

func (s *S3) LocalPath(string) (string, bool) { return "", false }

func (s *S3) Download(ctx context.Context, key string, w io.Writer, max int64) (int64, error) {
	ok, found := s.objectKey(key)
	if !found {
		return 0, ferr.New(ferr.ErrNotFound, "image not found")
	}
	obj, err := s.c.GetObject(ctx, s.bucket, ok, minio.GetObjectOptions{})
	if err != nil {
		return 0, s.err(ctx, err)
	}
	defer obj.Close()
	n, err := copyCapped(w, obj, max)
	if err != nil {
		if ferr.Kind(err) != ferr.ErrInternal { // already one of ours
			return n, err
		}
		return n, s.err(ctx, err)
	}
	return n, nil
}

// err maps an S3 error to ours. The message never carries keys or endpoints.
func (s *S3) err(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s3.IsNotFound(err) {
		return ferr.New(ferr.ErrNotFound, "image not found")
	}
	return ferr.Wrap(ferr.ErrUpstream, err, "the storage bucket did not answer")
}
