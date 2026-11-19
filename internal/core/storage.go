package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Storage is where backup objects live. Keys use forward slashes.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
	DeletePrefix(ctx context.Context, prefix string) error
	// Free returns the free bytes, or -1 when the storage cannot tell.
	Free() int64
	Test(ctx context.Context) error
}

type DestConfig struct {
	Path      string `json:"path,omitempty"`     // local
	Endpoint  string `json:"endpoint,omitempty"` // s3: host[:port]
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	UseSSL    bool   `json:"useSSL,omitempty"`
}

func OpenStorage(kind string, cfgJSON string, secret string) (Storage, error) {
	var c DestConfig
	if err := json.Unmarshal([]byte(cfgJSON), &c); err != nil {
		return nil, err
	}
	switch kind {
	case "local":
		if !filepath.IsAbs(c.Path) {
			return nil, errors.New("local destination needs an absolute folder path")
		}
		return &localStore{root: filepath.Clean(c.Path)}, nil
	case "s3":
		if c.Endpoint == "" || c.Bucket == "" {
			return nil, errors.New("S3 destination needs an endpoint and a bucket")
		}
		cl, err := minio.New(c.Endpoint, &minio.Options{
			Creds: credentials.NewStaticV4(c.AccessKey, secret, ""), Secure: c.UseSSL, Region: c.Region,
		})
		if err != nil {
			return nil, err
		}
		return &s3Store{cl: cl, bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
	}
	return nil, fmt.Errorf("unknown destination kind %q", kind)
}

type localStore struct{ root string }

func (l *localStore) file(key string) (string, error) {
	p := filepath.Join(l.root, filepath.FromSlash(key))
	if p != l.root && !strings.HasPrefix(p, l.root+string(os.PathSeparator)) {
		return "", errors.New("object key leaves the destination folder")
	}
	return p, nil
}

func (l *localStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	p, err := l.file(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, &ctxReader{ctx, r})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p + ".part")
		return n, err
	}
	return n, os.Rename(p+".part", p)
}

func (l *localStore) Open(_ context.Context, key string) (io.ReadSeekCloser, int64, error) {
	p, err := l.file(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *localStore) DeletePrefix(_ context.Context, prefix string) error {
	p, err := l.file(strings.TrimRight(prefix, "/"))
	if err != nil {
		return err
	}
	if p == l.root {
		return errors.New("refusing to delete the whole destination")
	}
	return os.RemoveAll(p)
}

func (l *localStore) Free() int64 {
	var st syscall.Statfs_t
	dir := l.root
	for {
		if err := syscall.Statfs(dir, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return -1
		}
		dir = parent
	}
}

func (l *localStore) Test(context.Context) error {
	if err := os.MkdirAll(l.root, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(l.root, ".write-test-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

type s3Store struct {
	cl             *minio.Client
	bucket, prefix string
}

func (s *s3Store) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	// Unknown length: multipart upload, 32 MiB parts (objects up to ~320 GB).
	info, err := s.cl.PutObject(ctx, s.bucket, s.key(key), r, -1, minio.PutObjectOptions{PartSize: 32 << 20})
	return info.Size, err
}

func (s *s3Store) Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	o, err := s.cl.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	st, err := o.Stat()
	if err != nil {
		o.Close()
		return nil, 0, err
	}
	return o, st.Size, nil
}

func (s *s3Store) DeletePrefix(ctx context.Context, prefix string) error {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return errors.New("refusing to delete the whole destination")
	}
	for o := range s.cl.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.key(prefix) + "/", Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
		if err := s.cl.RemoveObject(ctx, s.bucket, o.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *s3Store) Free() int64 { return -1 }

func (s *s3Store) Test(ctx context.Context) error {
	ok, err := s.cl.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	k := ".write-test"
	if _, err := s.Put(ctx, k, strings.NewReader("ok")); err != nil {
		return err
	}
	return s.cl.RemoveObject(ctx, s.bucket, s.key(k), minio.RemoveObjectOptions{})
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
