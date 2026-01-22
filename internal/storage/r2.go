package storage

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Storage struct {
	Client    *s3.Client
	Uploader  *manager.Uploader
	Bucket    string
	PublicURL string
}

func getContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))

	switch ext {
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".mp4":
		return "video/mp4"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".raw":
		return "application/octet-stream"

	// 2. Fallback to OS detection for anything else
	default:
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			return "application/octet-stream"
		}
		return ct
	}
}

func NewR2Storage(ctx context.Context, accountId, accessKeyId, secretAccessKey, bucket, publicUrl string) (*R2Storage, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyId, secretAccessKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to load SDK config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountId))
		// R2 doesn't always provide checksums in the way AWS SDK expects for multipart uploads, leading to warnings.
		// We disable the warning log specifically.
		o.DisableLogOutputChecksumValidationSkipped = true
	})

	// Initialize the Uploader once with optimal settings
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 10 * 1024 * 1024 // 10MB parts (Reduces API calls for 100MB files)
		u.Concurrency = 5             // Upload 5 parts at once
	})

	return &R2Storage{
		Client:    client,
		Uploader:  uploader,
		Bucket:    bucket,
		PublicURL: publicUrl,
	}, nil
}

// Save now uses the concurrent Uploader and detects Content-Type
func (s *R2Storage) Save(ctx context.Context, key string, data io.Reader) (string, error) {
	// We need to set the Content-Type for R2 to serve the files correctly.
	contentType := getContentType(key)
	var contentLength int64 = -1

	// Try to determine the size of the content
	if seeker, ok := data.(io.Seeker); ok {
		// Get current position
		currentPos, err := seeker.Seek(0, io.SeekCurrent)
		if err == nil {
			// Get end position
			endPos, err := seeker.Seek(0, io.SeekEnd)
			if err == nil {
				contentLength = endPos - currentPos
				// Reset to original position
				_, _ = seeker.Seek(currentPos, io.SeekStart)
			}
		}
	}

	// 1. If small file (< 20MB) and we know the size, use simple PutObject
	// This avoids multipart upload overhead and potential issues with R2
	if contentLength != -1 && contentLength < 20*1024*1024 {
		_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.Bucket),
			Key:           aws.String(key),
			Body:          data,
			ContentType:   aws.String(contentType),
			ContentLength: aws.Int64(contentLength),
		})
		if err != nil {
			return "", fmt.Errorf("failed to upload to R2 (simple): %w", err)
		}
	} else {
		// 2. Fallback to Uploader for large files or unknown size
		_, err := s.Uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.Bucket),
			Key:         aws.String(key),
			Body:        data,
			ContentType: aws.String(contentType),
		})
		if err != nil {
			return "", fmt.Errorf("failed to upload to R2 (multipart): %w", err)
		}
	}

	return s.GetURL(key), nil
}

func (s *R2Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	output, err := s.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download from R2: %w", err)
	}

	return output.Body, nil
}

func (s *R2Storage) GetURL(key string) string {
	if s.PublicURL == "" {
		return key
	}
	return fmt.Sprintf("%s/%s", s.PublicURL, key)
}

func (s *R2Storage) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	return err
}

func (s *R2Storage) DeleteFolder(ctx context.Context, key string) error {
	// We let the buckets lifecycle rules handle deleting old files
	return nil
}

func (s *R2Storage) IsLocal() bool {
	return false
}

func (s *R2Storage) StreamExists(ctx context.Context, key string) (bool, error) {
	// Check if any objects exist with the prefix
	listOutput, err := s.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.Bucket),
		Prefix:  aws.String(key),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list objects in R2: %w", err)
	}

	return len(listOutput.Contents) > 0, nil
}
