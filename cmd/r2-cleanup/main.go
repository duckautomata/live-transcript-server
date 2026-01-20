package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"gopkg.in/yaml.v3"
)

// Config structure matches config-example.yaml
type Config struct {
	Storage struct {
		Type string `yaml:"type"`
		R2   struct {
			AccountId       string `yaml:"accountId"`
			AccessKeyId     string `yaml:"accessKeyId"`
			SecretAccessKey string `yaml:"secretAccessKey"`
			Bucket          string `yaml:"bucket"`
			PublicUrl       string `yaml:"publicUrl"`
		} `yaml:"r2"`
	} `yaml:"storage"`
}

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

	// Read Config
	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config file %s: %v", *configPath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}

	if cfg.Storage.Type != "r2" {
		log.Fatalf("Config storage type is not 'r2'. It is '%s'. This script is for R2.", cfg.Storage.Type)
	}

	r2Config := cfg.Storage.R2
	if r2Config.AccountId == "" || r2Config.AccessKeyId == "" || r2Config.SecretAccessKey == "" || r2Config.Bucket == "" {
		log.Fatal("Missing R2 configuration in config file.")
	}

	// Initialize AWS Client
	ctx := context.TODO()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(r2Config.AccessKeyId, r2Config.SecretAccessKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		log.Fatalf("Unable to load SDK config: %v", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", r2Config.AccountId))
	})

	fmt.Println("Listing objects...")
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(r2Config.Bucket),
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

		var objects []types.ObjectIdentifier
		for _, obj := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: obj.Key})
		}

		fmt.Printf("Deleting batch of %d objects...\n", len(objects))
		_, err = client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(r2Config.Bucket),
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
