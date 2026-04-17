package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gogent/pkg/idgen"
	"io"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// FileInfo holds file metadata.
type FileInfo struct {
	Filename    string
	Size        int64
	ContentType string
	LastModifed time.Time
}

// S3FileStorage implements FileStorage using AWS S3.
type S3FileStorage struct {
	client             *s3.Client
	bucket             string
	basePath           string
	reliableUpload     bool
	reliableMaxRetries int
}

// S3Config holds S3 configuration.
type S3Config struct {
	Endpoint           string
	AccessKeyID        string
	SecretAccessKey    string
	Region             string
	Bucket             string
	BasePath           string
	UsePathStyle       bool // true for MinIO, false for AWS S3
	ReliableUpload     bool
	ReliableMaxRetries int
}

// NewS3FileStorage creates a new S3 file storage.
func NewS3FileStorage(ctx context.Context, cfg S3Config) (*S3FileStorage, error) {
	// Load AWS config
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID,
			cfg.SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = cfg.UsePathStyle
		}
	})

	return &S3FileStorage{
		client:             client,
		bucket:             cfg.Bucket,
		basePath:           cfg.BasePath,
		reliableUpload:     cfg.ReliableUpload,
		reliableMaxRetries: normalizeRetries(cfg.ReliableMaxRetries),
	}, nil
}

// Save stores a file and returns a stable s3://bucket/key location.
func (s *S3FileStorage) Save(filename string, reader io.Reader) (string, error) {
	return s.saveWithContext(context.Background(), filename, reader, "application/octet-stream")
}

func (s *S3FileStorage) saveWithContext(ctx context.Context, filename string, reader io.Reader, contentType string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		filename = "document.bin"
	}
	key := s.buildObjectKey(filename)

	if s.reliableUpload {
		body, readErr := io.ReadAll(reader)
		if readErr != nil {
			return "", fmt.Errorf("failed to read upload body: %w", readErr)
		}
		var err error
		for attempt := 1; attempt <= s.reliableMaxRetries; attempt++ {
			_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:      aws.String(s.bucket),
				Key:         aws.String(key),
				Body:        bytes.NewReader(body),
				ContentType: aws.String(contentType),
			})
			if err == nil {
				break
			}
			if attempt < s.reliableMaxRetries {
				time.Sleep(time.Duration(attempt*attempt) * 200 * time.Millisecond)
			}
		}
		if err != nil {
			return "", fmt.Errorf("failed to upload to S3 after retries: %w", err)
		}
	} else {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.bucket),
			Key:         aws.String(key),
			Body:        reader,
			ContentType: aws.String(contentType),
		})
		if err != nil {
			return "", fmt.Errorf("failed to upload to S3: %w", err)
		}
	}

	return s.buildURL(s.bucket, key), nil
}

// SaveWithMetadata saves a file with custom metadata.
func (s *S3FileStorage) SaveWithMetadata(ctx context.Context, filename string, reader io.Reader, contentType string, metadata map[string]string) (string, error) {
	key := s.buildObjectKey(filename)

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        reader,
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to S3 with metadata: %w", err)
	}

	return s.buildURL(s.bucket, key), nil
}

// GetDownloadURL generates a presigned download URL.
func (s *S3FileStorage) GetDownloadURL(ctx context.Context, filename string, expires time.Duration) (string, error) {
	_, key := s.resolveLocation(filename)

	presignClient := s3.NewPresignClient(s.client)
	req, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = expires
	})
	if err != nil {
		return "", fmt.Errorf("failed to presign URL: %w", err)
	}

	return req.URL, nil
}

// GetUploadURL generates a presigned upload URL (for direct client upload).
func (s *S3FileStorage) GetUploadURL(ctx context.Context, filename string, contentType string, expires time.Duration) (string, error) {
	key := s.buildObjectKey(filename)

	presignClient := s3.NewPresignClient(s.client)
	req, err := presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = expires
	})
	if err != nil {
		return "", fmt.Errorf("failed to presign upload URL: %w", err)
	}

	return req.URL, nil
}

// Delete deletes a file from S3 by s3:// URL or key.
func (s *S3FileStorage) Delete(storedPath string) error {
	bucket, key := s.resolveLocation(storedPath)
	if strings.TrimSpace(bucket) == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("invalid s3 stored path: %s", storedPath)
	}
	return s.deleteWithContext(context.Background(), bucket, key)
}

func (s *S3FileStorage) deleteWithContext(ctx context.Context, bucket string, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}
	return nil
}

// Load opens an object by s3:// URL or key.
func (s *S3FileStorage) Load(storedPath string) (io.ReadCloser, error) {
	bucket, key := s.resolveLocation(storedPath)
	if strings.TrimSpace(bucket) == "" || strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("invalid s3 stored path: %s", storedPath)
	}
	resp, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open s3 object: %w", err)
	}
	return resp.Body, nil
}

// Exists checks if a file exists in S3.
func (s *S3FileStorage) Exists(ctx context.Context, filename string) (bool, error) {
	_, key := s.resolveLocation(filename)

	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// GetFileInfo gets file metadata from S3.
func (s *S3FileStorage) GetFileInfo(ctx context.Context, filename string) (*FileInfo, error) {
	_, key := s.resolveLocation(filename)

	resp, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}

	return &FileInfo{
		Filename:    filename,
		Size:        aws.ToInt64(resp.ContentLength),
		ContentType: aws.ToString(resp.ContentType),
		LastModifed: aws.ToTime(resp.LastModified),
	}, nil
}

func (s *S3FileStorage) buildObjectKey(filename string) string {
	name := path.Base(strings.TrimSpace(filename))
	if name == "." || name == "/" || name == "" {
		name = "document.bin"
	}
	uniqueName := idgen.NextIDStr() + "_" + name
	if s.basePath == "" {
		return uniqueName
	}
	return strings.TrimRight(s.basePath, "/") + "/" + uniqueName
}

func (s *S3FileStorage) buildURL(bucket string, key string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, strings.TrimLeft(key, "/"))
}

func (s *S3FileStorage) resolveLocation(storedPath string) (string, string) {
	trimmed := strings.TrimSpace(storedPath)
	if strings.HasPrefix(trimmed, "s3://") {
		raw := strings.TrimPrefix(trimmed, "s3://")
		parts := strings.SplitN(raw, "/", 2)
		if len(parts) != 2 {
			return "", ""
		}
		return parts[0], parts[1]
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		// Backward compatibility with old absolute URL storage.
		prefix := "https://" + s.bucket + ".s3.amazonaws.com/"
		if strings.HasPrefix(trimmed, prefix) {
			return s.bucket, strings.TrimPrefix(trimmed, prefix)
		}
		return s.bucket, strings.TrimLeft(trimmed, "/")
	}
	return s.bucket, strings.TrimLeft(trimmed, "/")
}

// EnsureBucket implements BucketAdmin. It creates the bucket if it does not
// exist yet. Bucket-already-owned-by-you is treated as success; owned-by-another
// account is returned as an error.
func (s *S3FileStorage) EnsureBucket(ctx context.Context, bucketName string) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err == nil {
		return nil
	}
	var alreadyOwned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &alreadyOwned) {
		return nil // idempotent – we own the bucket already
	}
	var alreadyExists *types.BucketAlreadyExists
	if errors.As(err, &alreadyExists) {
		return fmt.Errorf("存储桶名称已被其他账户占用：%s", bucketName)
	}
	return fmt.Errorf("create bucket %q: %w", bucketName, err)
}

func normalizeRetries(retries int) int {
	if retries <= 0 {
		return 3
	}
	return retries
}
