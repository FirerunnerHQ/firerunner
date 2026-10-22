// Package s3store implements cacheserver.BlobStore on Amazon S3, so cache
// blobs and the entry index outlive the cache server that wrote them.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Store keeps blobs at <prefix>blobs/<id> and the index at <prefix>index.json.
// One prefix belongs to one cache server at a time: entry ids are allocated by
// the server, so two live servers on one prefix would overwrite each other.
type Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string
}

// New uses the default credential chain (an instance profile on EC2).
func New(ctx context.Context, bucket, prefix, region string) (*Store, error) {
	if bucket == "" {
		return nil, errors.New("s3store: bucket is required")
	}
	// Short dial and header timeouts keep an unreachable S3 from holding a job's
	// cache step; the body of a large transfer is bounded by its context instead.
	httpClient := awshttp.NewBuildableClient().WithDialerOptions(func(d *net.Dialer) {
		d.Timeout = 3 * time.Second
	}).WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = 10 * time.Second
	})
	// The SDK does not fall back to instance metadata for the region on its own.
	opts := []func(*config.LoadOptions) error{config.WithHTTPClient(httpClient), config.WithEC2IMDSRegion()}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3store: load AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Ranged reads of multipart objects can never validate a whole-object
		// checksum; the SDK would log that on every restore.
		o.DisableLogOutputChecksumValidationSkipped = true
	})
	return &Store{
		client: client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = 32 << 20
			u.Concurrency = 8
		}),
		bucket: bucket,
		prefix: prefix,
	}, nil
}

func (s *Store) blobKey(id uint64) string {
	return s.prefix + "blobs/" + strconv.FormatUint(id, 10)
}

func (s *Store) Put(ctx context.Context, id uint64, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.blobKey(id)),
		Body:   f,
	})
	return err
}

func (s *Store) Get(ctx context.Context, id uint64, offset, length int64) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.blobKey(id)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *Store) Delete(ctx context.Context, id uint64) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.blobKey(id)),
	})
	return err
}

func (s *Store) LoadIndex(ctx context.Context) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefix + "index.json"),
	})
	var missing *types.NoSuchKey
	if errors.As(err, &missing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *Store) SaveIndex(ctx context.Context, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.prefix + "index.json"),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}
