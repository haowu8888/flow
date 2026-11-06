package engine

import (
	"context"
	"fmt"
	"io"
	"strings"

	"flow/internal/blob"
	"flow/internal/model"
)

// 存到服务连接（对象存储）里的构建物，blob key 形如 ext/<连接 ID>/<Bucket>/<对象路径>；
// 其余 key 存在 Server 自己的 blob 存储中。

const extPrefix = "ext/"

func extKey(connID, bucket, object string) string {
	return extPrefix + connID + "/" + bucket + "/" + object
}

// blobFor 返回 key 所在的存储与其中的对象 key。
func (e *Engine) blobFor(ctx context.Context, key string) (blob.Store, string, error) {
	rest, ok := strings.CutPrefix(key, extPrefix)
	if !ok {
		return e.blobs, key, nil
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return nil, "", fmt.Errorf("invalid external blob key %q", key)
	}
	c, err := e.store.GetConnection(ctx, parts[0])
	if err != nil {
		return nil, "", fmt.Errorf("storage connection of %s: %w", key, err)
	}
	s, _, err := e.connS3(c, parts[1])
	return s, parts[2], err
}

// connS3 按对象存储服务连接创建 S3 客户端；bucket 为空时用连接的默认 Bucket。
func (e *Engine) connS3(c *model.ServiceConnection, bucket string) (*blob.S3, string, error) {
	if c.Type != "oss" {
		return nil, "", fmt.Errorf("%w: connection %q is %s, not object storage (oss)", ErrInvalid, c.Name, c.Type)
	}
	v, err := e.connValues(c)
	if err != nil {
		return nil, "", err
	}
	if bucket == "" {
		bucket = v["bucket"]
	}
	if bucket == "" {
		return nil, "", fmt.Errorf("%w: connection %q has no default bucket; set the bucket in the step", ErrInvalid, c.Name)
	}
	s, err := blob.NewS3(blob.S3Config{Endpoint: v["endpoint"], Region: v["region"], Bucket: bucket,
		AccessKey: v["access_key_id"], SecretKey: v["access_key_secret"], PathStyle: v["path_style"] == "true"})
	return s, bucket, err
}

// ArtifactStorage 返回存到对象存储连接的构建物的「连接名 / Bucket」，存在 Server 时为空。
func (e *Engine) ArtifactStorage(ctx context.Context, a *model.Artifact) string {
	rest, ok := strings.CutPrefix(a.BlobKey, extPrefix)
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return ""
	}
	name := parts[0]
	if c, err := e.store.GetConnection(ctx, parts[0]); err == nil {
		name = c.Name
	}
	return name + " / " + parts[1]
}

func (e *Engine) openBlob(ctx context.Context, key string) (io.ReadCloser, error) {
	s, k, err := e.blobFor(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.Open(ctx, k)
}

func (e *Engine) removeBlob(ctx context.Context, key string) error {
	s, k, err := e.blobFor(ctx, key)
	if err != nil {
		return err
	}
	return s.Delete(ctx, k)
}
