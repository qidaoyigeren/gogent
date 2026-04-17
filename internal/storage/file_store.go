package storage

import (
	"context"
	"fmt"
	"gogent/pkg/idgen"
	"io"
	"os"
	"path/filepath"
)

// FileStore abstracts document file persistence across local/S3 backends.
type FileStore interface {
	Save(fileName string, reader io.Reader) (string, error)
	Load(storedPath string) (io.ReadCloser, error)
	Delete(storedPath string) error
}

// BucketAdmin is an optional interface for storage backends that support
// per-knowledge-base bucket management (e.g. S3/MinIO).
type BucketAdmin interface {
	// EnsureBucket creates the named bucket if it does not already exist.
	// Returns an error only when the bucket is owned by a different account.
	EnsureBucket(ctx context.Context, bucketName string) error
}

// LocalFileStore stores files on the local filesystem.
type LocalFileStore struct {
	basePath string
}

// NewLocalFileStore creates a file store with the given base directory.
func NewLocalFileStore(basePath string) *LocalFileStore {
	if basePath == "" {
		basePath = "data/uploads"
	}
	_ = os.MkdirAll(basePath, 0755)
	return &LocalFileStore{basePath: basePath}
}

// Save writes the reader content to a unique directory under basePath.
// Returns the relative stored path.
func (s *LocalFileStore) Save(fileName string, reader io.Reader) (string, error) {
	dir := idgen.NextIDStr()
	fullDir := filepath.Join(s.basePath, dir)
	if err := os.MkdirAll(fullDir, 0755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}

	filePath := filepath.Join(fullDir, fileName)
	f, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, reader); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	// Return relative path from basePath
	return filepath.Join(dir, fileName), nil
}

// Load opens a stored file for reading.
func (s *LocalFileStore) Load(storedPath string) (io.ReadCloser, error) {
	fullPath := filepath.Join(s.basePath, storedPath)
	return os.Open(fullPath)
}

// FullPath returns the absolute path for a stored file.
func (s *LocalFileStore) FullPath(storedPath string) string {
	return filepath.Join(s.basePath, storedPath)
}

// Delete removes a stored file.
func (s *LocalFileStore) Delete(storedPath string) error {
	fullPath := filepath.Join(s.basePath, storedPath)
	return os.Remove(fullPath)
}
