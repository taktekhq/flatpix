// Package s3 builds the S3 client shared by the S3 origin and the S3 derived
// cache. It speaks plain S3, so AWS S3, Cloudflare R2, MinIO and Google Cloud
// Storage (interoperability mode) all work.
package s3

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config is how to reach an S3-compatible service.
type Config struct {
	// Endpoint is a URL such as https://acct.r2.cloudflarestorage.com or
	// http://minio:9000. Empty means AWS S3.
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

// Client builds a client. With no keys it reads public objects anonymously.
func (c Config) Client() (*minio.Client, error) {
	endpoint, secure := c.Endpoint, true
	lookup := minio.BucketLookupAuto
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	} else {
		lookup = minio.BucketLookupPath // MinIO, R2 and GCS interop all take path style
		if strings.Contains(endpoint, "://") {
			u, err := url.Parse(endpoint)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("s3: bad endpoint %q", c.Endpoint)
			}
			endpoint, secure = u.Host, u.Scheme == "https"
		}
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	return minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
}

// IsNotFound reports whether err is S3 saying the object does not exist.
func IsNotFound(err error) bool {
	var r minio.ErrorResponse
	if errors.As(err, &r) {
		return r.StatusCode == http.StatusNotFound || r.Code == "NoSuchKey" || r.Code == "NotFound"
	}
	return false
}
