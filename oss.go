package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

const archivePrefix = "deplyt/photolib/"

type jarArchive struct {
	client *oss.Client
	bucket string
}

func newArchive(values map[string]string) (jarArchive, error) {
	for _, key := range []string{"OSS_BUCKET", "OSS_REGION", "OSS_ACCESS_KEY_ID", "OSS_ACCESS_KEY_SECRET"} {
		if values[key] == "" {
			return jarArchive{}, fmt.Errorf("%s is required", key)
		}
	}
	cfg := oss.LoadDefaultConfig().
		WithRegion(values["OSS_REGION"]).
		WithUseAccelerateEndpoint(true).
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			values["OSS_ACCESS_KEY_ID"], values["OSS_ACCESS_KEY_SECRET"]))
	return jarArchive{client: oss.NewClient(cfg), bucket: values["OSS_BUCKET"]}, nil
}

func (a jarArchive) upload(ctx context.Context, key, sha256 string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	_, err = a.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:          oss.Ptr(a.bucket),
		Key:             oss.Ptr(key),
		ContentLength:   oss.Ptr(info.Size()),
		ContentType:     oss.Ptr("application/java-archive"),
		ForbidOverwrite: oss.Ptr("true"),
		Acl:             oss.ObjectACLPrivate,
		Metadata:        map[string]string{"sha256": sha256},
		Body:            file,
	})
	return err
}

func (a jarArchive) downloadURL(ctx context.Context, key string) (string, error) {
	result, err := a.client.Presign(ctx, &oss.GetObjectRequest{Bucket: oss.Ptr(a.bucket), Key: oss.Ptr(key)},
		func(options *oss.PresignOptions) { options.Expires = deployTimeout })
	if err != nil {
		return "", err
	}
	return result.URL, nil
}
