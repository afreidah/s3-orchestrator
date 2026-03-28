//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

func newBenchS3Client() *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + proxyAddr),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})
}

func BenchmarkPutObject(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1 << 10},
		{"100KB", 100 << 10},
		{"1MB", 1 << 20},
		{"10MB", 10 << 20},
	}

	client := newBenchS3Client()
	ctx := context.Background()

	for _, sz := range sizes {
		data := bytes.Repeat([]byte("X"), sz.size)
		b.Run(sz.name, func(b *testing.B) {
			b.SetBytes(int64(sz.size))
			i := 0
			for b.Loop() {
				key := fmt.Sprintf("bench-put/%s-%d", sz.name, i)
				i++
				_, err := client.PutObject(ctx, &s3.PutObjectInput{
					Bucket:        aws.String(virtualBucket),
					Key:           aws.String(key),
					Body:          bytes.NewReader(data),
					ContentLength: aws.Int64(int64(sz.size)),
				})
				if err != nil {
					b.Fatalf("PutObject: %v", err)
				}
			}
		})
	}
}

func BenchmarkListObjects(b *testing.B) {
	client := newBenchS3Client()
	ctx := context.Background()

	// Pre-populate objects for listing.
	const populateCount = 200
	data := []byte("list-bench-payload")
	for i := 0; i < populateCount; i++ {
		key := fmt.Sprintf("bench-list/%04d.txt", i)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data))),
		})
		if err != nil {
			b.Fatalf("populate PutObject: %v", err)
		}
	}

	prefix := "bench-list/"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:  aws.String(virtualBucket),
			Prefix:  aws.String(prefix),
			MaxKeys: aws.Int32(100),
		})
		if err != nil {
			b.Fatalf("ListObjectsV2: %v", err)
		}
	}
}

func BenchmarkRebalance(b *testing.B) {
	client := newBenchS3Client()
	ctx := context.Background()

	// Pre-populate small objects.
	const populateCount = 50
	data := []byte("rebalance-bench-payload")
	for i := 0; i < populateCount; i++ {
		key := fmt.Sprintf("bench-rebal/%04d.txt", i)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data))),
		})
		if err != nil {
			b.Fatalf("populate PutObject: %v", err)
		}
	}

	cfg := config.RebalanceConfig{
		Strategy:    "spread",
		BatchSize:   50,
		Threshold:   0.0, // always trigger
		Concurrency: 5,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := testManager.Rebalancer.Rebalance(ctx, cfg)
		if err != nil {
			b.Fatalf("Rebalance: %v", err)
		}
	}
}
