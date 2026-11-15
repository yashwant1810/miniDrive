package s3client

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Client defines the interface for all S3/MinIO operations.
// Handlers depend on this interface, not the concrete implementation.
type S3Client interface {
	GeneratePresignedUploadURL(ctx context.Context, key string, expiry time.Duration) (string, error)
	GeneratePresignedDownloadURL(ctx context.Context, key string, expiry time.Duration) (string, error)
	HeadChunk(ctx context.Context, key string) (int64, error)
	DeleteChunk(ctx context.Context, key string) error
}

// Client implements S3Client using the AWS SDK.
type Client struct {
	client       *s3.Client
	presigner    *s3.PresignClient
	bucket       string
}

// New creates a new S3/MinIO client.
func New(endpoint, bucket, region, accessKey, secretKey string, usePathStyle bool) (*Client, error) {
	// Build a custom AWS config pointing at MinIO (or real S3)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load aws config: %w", err)
	}

	// Create the S3 client with custom endpoint (MinIO) and path-style addressing
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = usePathStyle // MinIO requires path-style; AWS S3 uses virtual-hosted
	})

	return &Client{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    bucket,
	}, nil
}

// GeneratePresignedUploadURL returns a presigned PUT URL for uploading a chunk.
func (c *Client) GeneratePresignedUploadURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	req, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("failed to generate upload url: %w", err)
	}
	return req.URL, nil
}

// GeneratePresignedDownloadURL returns a presigned GET URL for downloading a chunk.
func (c *Client) GeneratePresignedDownloadURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	req, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("failed to generate download url: %w", err)
	}
	return req.URL, nil
}

// HeadChunk checks if a chunk exists in S3 and returns its size.
// Returns an error if the chunk doesn't exist.
func (c *Client) HeadChunk(ctx context.Context, key string) (int64, error) {
	out, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("chunk not found in s3: %w", err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}

// DeleteChunk removes a chunk from S3.
func (c *Client) DeleteChunk(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete chunk: %w", err)
	}
	return nil
}
