package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	sess "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/containerd/containerd/content"
	"github.com/moby/buildkit/cache/remotecache"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	attrBucket           = "bucket"
	attrRegion           = "region"
	attrRole             = "role"
	attrName             = "name"
	attrTouchRefresh     = "touch_refresh"
	attrEndpointURL      = "endpoint_url"
	attrAccessKeyID      = "access_key_id"
	attrSecretAccessKey  = "secret_access_key"
	attrS3ForcePathStyle = "s3_force_path_style"
)

type Config struct {
	Bucket           string
	Region           string
	Role             string
	Names            []string
	TouchRefresh     time.Duration
	EndpointURL      string
	AccessKeyID      string
	SecretAccessKey  string
	S3ForcePathStyle bool
}

func manifestKey(name string) string {
	return "manifests/" + name
}

func blobKey(dgst digest.Digest) string {
	return "blobs/" + dgst.String()
}

func getConfig(attrs map[string]string) (Config, error) {
	bucket, ok := attrs[attrBucket]
	if !ok {
		bucket, ok = os.LookupEnv("AWS_BUCKET")
		if !ok {
			return Config{}, errors.Errorf("bucket ($AWS_BUCKET) not set for s3 cache")
		}
	}

	region, ok := attrs[attrRegion]
	if !ok {
		region, ok = os.LookupEnv("AWS_REGION")
		if !ok {
			return Config{}, errors.Errorf("region ($AWS_REGION) not set for s3 cache")
		}
	}

	role, ok := attrs[attrRole]
	if !ok {
		role = os.Getenv("AWS_ROLE_ARN")
	}

	names := []string{"buildkit"}
	name, ok := attrs[attrName]
	if ok {
		splittedNames := strings.Split(name, ";")
		if len(splittedNames) > 0 {
			names = splittedNames
		}
	}

	touchRefresh := 24 * time.Hour

	touchRefreshStr, ok := attrs[attrTouchRefresh]
	if ok {
		touchRefreshFromUser, err := time.ParseDuration(touchRefreshStr)
		if err == nil {
			touchRefresh = touchRefreshFromUser
		}
	}

	endpointURL := attrs[attrEndpointURL]
	accessKeyID := attrs[attrAccessKeyID]
	secretAccessKey := attrs[attrSecretAccessKey]

	s3ForcePathStyle := false
	s3ForcePathStyleStr, ok := attrs[attrS3ForcePathStyle]
	if ok {
		s3ForcePathStyleUser, err := strconv.ParseBool(s3ForcePathStyleStr)
		if err == nil {
			s3ForcePathStyle = s3ForcePathStyleUser
		}
	}

	return Config{
		Bucket:           bucket,
		Region:           region,
		Role:             role,
		Names:            names,
		TouchRefresh:     touchRefresh,
		EndpointURL:      endpointURL,
		AccessKeyID:      accessKeyID,
		SecretAccessKey:  secretAccessKey,
		S3ForcePathStyle: s3ForcePathStyle,
	}, nil
}

// ResolveCacheExporterFunc for s3 cache exporter.
func ResolveCacheExporterFunc() remotecache.ResolveCacheExporterFunc {
	return func(_ context.Context, _ session.Group, attrs map[string]string) (remotecache.Exporter, error) {
		config, err := getConfig(attrs)
		if err != nil {
			return nil, err
		}

		return NewExporter(config)
	}
}

type exporter struct {
	solver.CacheExporterTarget
	chains   *v1.CacheChains
	s3Client *s3ClientWrapper
	config   Config
}

func NewExporter(config Config) (remotecache.Exporter, error) {
	s3Client, err := newS3ClientWrapper(config)
	if err != nil {
		return nil, err
	}
	cc := v1.NewCacheChains()
	return &exporter{CacheExporterTarget: cc, chains: cc, s3Client: s3Client, config: config}, nil
}

func (e *exporter) Config() remotecache.Config {
	return remotecache.Config{
		Compression: compression.New(compression.Default),
	}
}

func (e *exporter) Finalize(ctx context.Context) (map[string]string, error) {
	cacheConfig, descs, err := e.chains.Marshal(ctx)
	if err != nil {
		return nil, err
	}

	for i, l := range cacheConfig.Layers {
		dgstPair, ok := descs[l.Blob]
		if !ok {
			return nil, errors.Errorf("missing blob %s", l.Blob)
		}
		if dgstPair.Descriptor.Annotations == nil {
			return nil, errors.Errorf("invalid descriptor without annotations")
		}
		var diffID digest.Digest
		v, ok := dgstPair.Descriptor.Annotations["containerd.io/uncompressed"]
		if !ok {
			return nil, errors.Errorf("invalid descriptor without uncompressed annotation")
		}
		dgst, err := digest.Parse(v)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse uncompressed annotation")
		}
		diffID = dgst

		key := blobKey(dgstPair.Descriptor.Digest)
		exists, err := e.s3Client.exists(key)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to check file presence in cache")
		}
		if exists != nil {
			if time.Since(*exists) > e.config.TouchRefresh {
				err = e.s3Client.touch(key)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to touch file")
				}
			}
		} else {
			layerDone := oneOffProgress(ctx, fmt.Sprintf("writing layer %s", l.Blob))
			bytes, err := content.ReadBlob(ctx, dgstPair.Provider, dgstPair.Descriptor)
			if err != nil {
				return nil, layerDone(err)
			}
			if err := e.s3Client.saveMutable(key, bytes); err != nil {
				return nil, layerDone(errors.Wrap(err, "error writing layer blob"))
			}

			layerDone(nil)
		}

		la := &v1.LayerAnnotations{
			DiffID:    diffID,
			Size:      dgstPair.Descriptor.Size,
			MediaType: dgstPair.Descriptor.MediaType,
		}
		if v, ok := dgstPair.Descriptor.Annotations["buildkit/createdat"]; ok {
			var t time.Time
			if err := (&t).UnmarshalText([]byte(v)); err != nil {
				return nil, err
			}
			la.CreatedAt = t.UTC()
		}
		cacheConfig.Layers[i].Annotations = la
	}

	dt, err := json.Marshal(cacheConfig)
	if err != nil {
		return nil, err
	}

	for _, name := range e.config.Names {
		if err := e.s3Client.saveMutable(manifestKey(name), dt); err != nil {
			return nil, errors.Wrapf(err, "error writing manifest: %s", name)
		}
	}
	return nil, nil
}

// ResolveCacheImporterFunc for s3 cache importer.
func ResolveCacheImporterFunc() remotecache.ResolveCacheImporterFunc {
	return func(_ context.Context, _ session.Group, attrs map[string]string) (remotecache.Importer, ocispecs.Descriptor, error) {
		config, err := getConfig(attrs)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}
		s3Client, err := newS3ClientWrapper(config)
		if err != nil {
			return nil, ocispecs.Descriptor{}, err
		}
		return &importer{s3Client, config}, ocispecs.Descriptor{}, nil
	}
}

type importer struct {
	s3Client *s3ClientWrapper
	config   Config
}

func (i *importer) makeDescriptorProviderPair(l v1.CacheLayer) (*v1.DescriptorProviderPair, error) {
	if l.Annotations == nil {
		return nil, errors.Errorf("cache layer with missing annotations")
	}
	if l.Annotations.DiffID == "" {
		return nil, errors.Errorf("cache layer with missing diffid")
	}
	annotations := map[string]string{}
	annotations["containerd.io/uncompressed"] = l.Annotations.DiffID.String()
	if !l.Annotations.CreatedAt.IsZero() {
		txt, err := l.Annotations.CreatedAt.MarshalText()
		if err != nil {
			return nil, err
		}
		annotations["buildkit/createdat"] = string(txt)
	}
	return &v1.DescriptorProviderPair{
		Provider: i.s3Client,
		Descriptor: ocispecs.Descriptor{
			MediaType:   l.Annotations.MediaType,
			Digest:      l.Blob,
			Size:        l.Annotations.Size,
			Annotations: annotations,
		},
	}, nil
}

func (i *importer) load() (*v1.CacheChains, error) {
	var config v1.CacheConfig
	found, err := i.s3Client.getManifest(manifestKey(i.config.Names[0]), &config)
	if err != nil {
		return nil, err
	}
	if !found {
		return v1.NewCacheChains(), nil
	}

	allLayers := v1.DescriptorProvider{}

	for _, l := range config.Layers {
		dpp, err := i.makeDescriptorProviderPair(l)
		if err != nil {
			return nil, err
		}
		allLayers[l.Blob] = *dpp
	}

	cc := v1.NewCacheChains()
	if err := v1.ParseConfig(config, allLayers, cc); err != nil {
		return nil, err
	}
	return cc, nil
}

func (i *importer) Resolve(ctx context.Context, _ ocispecs.Descriptor, id string, w worker.Worker) (solver.CacheManager, error) {
	cc, err := i.load()
	if err != nil {
		return nil, err
	}

	keysStorage, resultStorage, err := v1.NewCacheKeyStorage(cc, w)
	if err != nil {
		return nil, err
	}

	return solver.NewCacheManager(ctx, id, keysStorage, resultStorage), nil
}

type readerAt struct {
	ReaderAtCloser
	size int64
}

func (r *readerAt) Size() int64 {
	return r.size
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.NewFromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}

type s3ClientWrapper struct {
	awsClient *s3.S3
	uploader  *s3manager.Uploader
	bucket    string
}

func newS3ClientWrapper(config Config) (*s3ClientWrapper, error) {
	s, err := sess.NewSession(&aws.Config{Region: &config.Region, Endpoint: &config.EndpointURL, S3ForcePathStyle: &config.S3ForcePathStyle})
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	var creds *credentials.Credentials
	if config.Role != "" {
		creds = stscreds.NewCredentials(s, config.Role)
	} else if config.AccessKeyID != "" && config.SecretAccessKey != "" {
		creds = credentials.NewStaticCredentials(config.AccessKeyID, config.SecretAccessKey, "")
	}

	awsClient := s3.New(s, &aws.Config{Credentials: creds})

	return &s3ClientWrapper{
		awsClient: awsClient,
		uploader:  s3manager.NewUploaderWithClient(awsClient),
		bucket:    config.Bucket,
	}, nil
}

func (s3Client *s3ClientWrapper) ReaderAt(ctx context.Context, desc ocispecs.Descriptor) (content.ReaderAt, error) {
	readerAtCloser := toReaderAtCloser(func(offset int64) (io.ReadCloser, error) {
		return s3Client.getReader(blobKey(desc.Digest))
	})
	return &readerAt{ReaderAtCloser: readerAtCloser, size: desc.Size}, nil
}

func (s3Client *s3ClientWrapper) getManifest(key string, config *v1.CacheConfig) (bool, error) {
	output, err := s3Client.awsClient.GetObject(&s3.GetObjectInput{
		Bucket: &s3Client.bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	defer output.Body.Close()

	decoder := json.NewDecoder(output.Body)
	if err := decoder.Decode(config); err != nil {
		return false, errors.WithStack(err)
	}

	return true, nil
}

func (s3Client *s3ClientWrapper) getReader(key string) (io.ReadCloser, error) {
	output, err := s3Client.awsClient.GetObject(&s3.GetObjectInput{
		Bucket: &s3Client.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func (s3Client *s3ClientWrapper) saveMutable(key string, value []byte) error {
	_, err := s3Client.uploader.Upload(&s3manager.UploadInput{
		Bucket: &s3Client.bucket,
		Key:    &key,
		Body:   bytes.NewReader(value),
	})
	return err
}

func (s3Client *s3ClientWrapper) exists(key string) (*time.Time, error) {
	head, err := s3Client.awsClient.HeadObject(&s3.HeadObjectInput{
		Bucket: &s3Client.bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return head.LastModified, nil
}

func (s3Client *s3ClientWrapper) touch(key string) error {
	copySource := fmt.Sprintf("%s/%s", s3Client.bucket, key)
	_, err := s3Client.awsClient.CopyObject(&s3.CopyObjectInput{
		Bucket:     &s3Client.bucket,
		CopySource: &copySource,
		Key:        &key,
	})
	return err
}

func isNotFound(err error) bool {
	var awsErr awserr.Error
	return errors.As(err, &awsErr) && awsErr.Code() == s3.ErrCodeNoSuchKey || awsErr.Code() == "NotFound"
}
