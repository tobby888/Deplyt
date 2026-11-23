package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

const archivePrefix = "deplyt/photolib/"

type jarArchive struct {
	uploadClient     *oss.Client
	productionClient *oss.Client
	bucket           string
}

func newArchive(values map[string]string) (jarArchive, error) {
	for _, key := range []string{"OSS_BUCKET", "OSS_REGION"} {
		if values[key] == "" {
			return jarArchive{}, fmt.Errorf("%s is required", key)
		}
	}
	productionEndpoint := values["OSS_PRODUCTION_ENDPOINT"]
	if productionEndpoint != "" {
		origin, err := httpsOrigin(productionEndpoint)
		if err != nil {
			return jarArchive{}, errors.New("OSS_PRODUCTION_ENDPOINT must be an HTTPS origin")
		}
		for label := range strings.SplitSeq(strings.TrimPrefix(origin, "https://"), ".") {
			if label == "oss-accelerate" || label == "oss-accelerate-overseas" {
				return jarArchive{}, errors.New("OSS_PRODUCTION_ENDPOINT must not use an OSS transfer acceleration endpoint")
			}
		}
		productionEndpoint = origin
	}
	id, secret, role := values["OSS_ACCESS_KEY_ID"], values["OSS_ACCESS_KEY_SECRET"], values["OSS_ECS_RAM_ROLE"]
	var provider credentials.CredentialsProvider
	if role != "" {
		if id != "" || secret != "" {
			return jarArchive{}, errors.New("configure either OSS_ECS_RAM_ROLE or OSS AccessKey credentials")
		}
		provider = credentials.NewEcsRoleCredentialsProvider(credentials.EcsRamRole(role))
	} else {
		if id == "" || secret == "" {
			return jarArchive{}, errors.New("OSS archiving requires OSS_ECS_RAM_ROLE or both OSS AccessKey settings")
		}
		provider = credentials.NewStaticCredentialsProvider(id, secret)
	}
	cfg := oss.LoadDefaultConfig().
		WithRegion(values["OSS_REGION"]).
		WithCredentialsProvider(provider)
	uploadConfig := *cfg
	return jarArchive{
		uploadClient:     oss.NewClient(uploadConfig.WithUseAccelerateEndpoint(true)),
		productionClient: oss.NewClient(cfg.WithEndpoint(productionEndpoint)),
		bucket:           values["OSS_BUCKET"],
	}, nil
}

func (a jarArchive) upload(ctx context.Context, key, sha256 string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	contentType := "application/java-archive"
	if strings.HasSuffix(key, ".tar.gz") {
		contentType = "application/gzip"
	}
	_, err = a.uploadClient.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:          oss.Ptr(a.bucket),
		Key:             oss.Ptr(key),
		ContentLength:   oss.Ptr(info.Size()),
		ContentType:     oss.Ptr(contentType),
		ForbidOverwrite: oss.Ptr("true"),
		Acl:             oss.ObjectACLPrivate,
		Metadata:        map[string]string{"sha256": sha256},
		Body:            file,
	})
	return err
}

func (a jarArchive) downloadURL(ctx context.Context, key string) (string, error) {
	result, err := a.productionClient.Presign(ctx, &oss.GetObjectRequest{Bucket: oss.Ptr(a.bucket), Key: oss.Ptr(key)},
		func(options *oss.PresignOptions) { options.Expires = deployTimeout })
	if err != nil {
		return "", err
	}
	return result.URL, nil
}
