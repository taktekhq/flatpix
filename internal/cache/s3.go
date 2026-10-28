// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Taktek and Nizar Mahmoud

package cache

import (
	"context"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/taktekhq/flatpix/internal/s3"
	"github.com/taktekhq/flatpix/internal/sniff"
)

// S3 stores outputs in a bucket: <prefix><first two hex>/<key>.
type S3 struct {
	c      *minio.Client
	bucket string
	prefix string
}

// NewS3 opens a bucket as a cache.
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

func (s *S3) objectKey(key string) string { return s.prefix + key[:2] + "/" + key }

func (s *S3) Get(ctx context.Context, key string) (*Entry, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, s.objectKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		if s3.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if st.Size == 0 {
		obj.Close()
		return nil, nil
	}
	return &Entry{Body: obj, Size: st.Size, ContentType: st.ContentType}, nil
}

func (s *S3) Put(ctx context.Context, key, srcPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	head := make([]byte, sniff.HeadSize)
	n, _ := f.Read(head)
	f.Close()
	_, err = s.c.FPutObject(ctx, s.bucket, s.objectKey(key), srcPath, minio.PutObjectOptions{
		ContentType:  sniff.Sniff(head[:n]).ContentType(),
		CacheControl: "public, max-age=31536000, immutable",
	})
	return err
}
