// r2-cleanup bulk-deletes objects from the R2 bucket by raw key prefix. It
// exists for manual cleanup jobs that don't map to a stream folder (use the
// admin UI's delete-with-media for those): arbitrary prefixes, or the whole
// bucket via -prefix '*'.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"live-transcript-server/internal/config"
	"live-transcript-server/internal/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	prefixPtr := flag.String("prefix", "", "Prefix to delete (Required). Use '*' or '/' for root/all.")
	flag.Parse()

	if *prefixPtr == "" {
		flag.Usage()
		log.Fatal("Error: -prefix is required.")
	}

	prefix := *prefixPtr
	if prefix == "*" || prefix == "/" {
		prefix = ""
		fmt.Println("Warning: You are about to delete EVERYTHING in the bucket.")
	} else {
		fmt.Printf("Prefix set to: %s\n", prefix)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if cfg.Storage.Type != "r2" {
		log.Fatalf("Config storage type is not 'r2'. It is '%s'. This script is for R2.", cfg.Storage.Type)
	}
	r2cfg := cfg.Storage.R2
	if r2cfg.AccountId == "" || r2cfg.AccessKeyId == "" || r2cfg.SecretAccessKey == "" || r2cfg.Bucket == "" {
		log.Fatal("Missing R2 configuration in config file.")
	}

	ctx := context.Background()
	r2, err := storage.NewR2Storage(ctx, r2cfg.AccountId, r2cfg.AccessKeyId, r2cfg.SecretAccessKey, r2cfg.Bucket, r2cfg.PublicUrl)
	if err != nil {
		log.Fatalf("Unable to initialize R2 client: %v", err)
	}

	// Raw prefix deletion (deliberately NOT R2Storage.DeleteFolder, which is
	// folder-scoped and appends a trailing slash): this tool matches any key
	// prefix, including the empty one.
	fmt.Println("Listing objects...")
	paginator := s3.NewListObjectsV2Paginator(r2.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(r2.Bucket),
		Prefix: aws.String(prefix),
	})

	totalDeleted := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Fatalf("Failed to list objects: %v", err)
		}
		if len(page.Contents) == 0 {
			continue
		}

		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: obj.Key})
		}

		fmt.Printf("Deleting batch of %d objects...\n", len(objects))
		_, err = r2.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(r2.Bucket),
			Delete: &types.Delete{
				Objects: objects,
				Quiet:   aws.Bool(true),
			},
		})
		if err != nil {
			log.Printf("Failed to delete batch: %v", err)
		} else {
			totalDeleted += len(objects)
		}
	}

	fmt.Printf("Done. Total objects deleted: %d\n", totalDeleted)
}
