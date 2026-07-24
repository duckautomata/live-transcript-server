package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type R2Storage struct {
	Client    *s3.Client
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

// ensureTrailingSlash guards a listing prefix against prefix aliasing:
// "chan/123" also matches "chan/1234". Callers may pass either form; the
// slash is appended only when absent.
func ensureTrailingSlash(prefix string) string {
	if !strings.HasSuffix(prefix, "/") {
		return prefix + "/"
	}
	return prefix
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

	return &R2Storage{
		Client:    client,
		Bucket:    bucket,
		PublicURL: publicUrl,
	}, nil
}

// Save uploads to R2.
func (s *R2Storage) Save(ctx context.Context, key string, data io.Reader, contentLength int64) (string, error) {
	// We need to set the Content-Type for R2 to serve the files correctly.
	contentType := getContentType(key)

	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.Bucket),
		Key:           aws.String(key),
		Body:          data,
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(contentLength),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to R2: %w", err)
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

// DeleteFolder deletes every object under the given prefix, in batches of up
// to 1000 (the DeleteObjects limit, which is also the list page size).
func (s *R2Storage) DeleteFolder(ctx context.Context, key string) error {
	prefix := ensureTrailingSlash(key)

	paginator := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.Bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list objects in R2: %w", err)
		}

		if len(page.Contents) == 0 {
			continue
		}

		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: obj.Key})
		}

		output, err := s.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.Bucket),
			Delete: &types.Delete{
				Objects: objects,
				// Quiet mode: the response lists only the objects that
				// failed to delete.
				Quiet: aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to delete batch from R2: %w", err)
		}
		if len(output.Errors) > 0 {
			for _, e := range output.Errors {
				slog.Error("failed to delete R2 object", "key", aws.ToString(e.Key), "code", aws.ToString(e.Code), "message", aws.ToString(e.Message))
			}
			return fmt.Errorf("failed to delete %d object(s) under prefix %s from R2", len(output.Errors), prefix)
		}
	}

	return nil
}

func (s *R2Storage) IsLocal() bool {
	return false
}

func (s *R2Storage) StreamExists(ctx context.Context, key string) (bool, error) {
	// Probe with a trailing slash so "chan/123" cannot match "chan/1234" —
	// a false positive here can permanently skip pruning a stream.
	prefix := ensureTrailingSlash(key)

	// Check if any objects exist with the prefix
	listOutput, err := s.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.Bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list objects in R2: %w", err)
	}

	return len(listOutput.Contents) > 0, nil
}
