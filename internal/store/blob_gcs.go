package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Default endpoints for GCSBlob.
const (
	gcsMetadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	gcsStorageURL       = "https://storage.googleapis.com"
	// tokenSlack refreshes the access token this long before it expires.
	tokenSlack = 60 * time.Second
	// maxErrorBody bounds how much of a GCS error response is copied into an error message.
	maxErrorBody = 512
)

// GCSBlob stores the snapshot as one object in a Google Cloud Storage bucket using the
// JSON API and the Cloud Run metadata-server token (no SDK).
//
//	token:    GET http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token
//	          header Metadata-Flavor: Google  -> {"access_token","expires_in"}; cached until 60 s before expiry
//	upload:   POST https://storage.googleapis.com/upload/storage/v1/b/{bucket}/o?uploadType=media&name={object}[&ifGenerationMatch=G]
//	          -> 200 {"generation":"123"}; 412 -> ErrGenerationMismatch
//	download: GET  https://storage.googleapis.com/storage/v1/b/{bucket}/o/{object}?alt=media&generation=G (404 -> ErrNoObject)
//	metadata: GET  https://storage.googleapis.com/storage/v1/b/{bucket}/o/{object}           -> {"generation":"123"}
type GCSBlob struct {
	Bucket string
	Object string
	// HTTP is the client for both metadata and storage calls; nil means a client with a
	// 30 s timeout.
	HTTP *http.Client
	// MetadataURL overrides the token endpoint (tests); "" means the Cloud Run default.
	MetadataURL string
	// StorageURL overrides https://storage.googleapis.com (tests).
	StorageURL string

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewGCSBlob returns a GCSBlob for gs://bucket/object.
func NewGCSBlob(bucket, object string) *GCSBlob {
	return &GCSBlob{Bucket: bucket, Object: object}
}

func (b *GCSBlob) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (b *GCSBlob) storageURL() string {
	if b.StorageURL != "" {
		return b.StorageURL
	}
	return gcsStorageURL
}

func (b *GCSBlob) metadataURL() string {
	if b.MetadataURL != "" {
		return b.MetadataURL
	}
	return gcsMetadataTokenURL
}

// objectURL is the JSON API URL of the object (metadata; add alt=media for content).
func (b *GCSBlob) objectURL() string {
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s", b.storageURL(), url.PathEscape(b.Bucket), url.PathEscape(b.Object))
}

// accessToken returns a cached metadata-server token, refreshing it near expiry.
func (b *GCSBlob) accessToken(ctx context.Context) (string, error) {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	if b.token != "" && time.Now().Before(b.tokenExpiry.Add(-tokenSlack)) {
		return b.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.metadataURL(), nil)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := b.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server: %s: %s", resp.Status, readErrorBody(resp.Body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("metadata server returned an empty token")
	}
	b.token = tok.AccessToken
	b.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return b.token, nil
}

// do sends an authenticated request.
func (b *GCSBlob) do(ctx context.Context, method, rawURL string, body io.Reader, contentType string) (*http.Response, error) {
	token, err := b.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := b.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, rawURL, err)
	}
	return resp, nil
}

// Generation fetches the object's current generation from its metadata (no download).
// Returns ErrNoObject when the object does not exist.
func (b *GCSBlob) Generation(ctx context.Context) (int64, error) {
	resp, err := b.do(ctx, http.MethodGet, b.objectURL(), nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, ErrNoObject
	default:
		return 0, fmt.Errorf("object metadata: %s: %s", resp.Status, readErrorBody(resp.Body))
	}
	return decodeGeneration(resp.Body)
}

// Get implements Blob: metadata for the generation, then the content of exactly that
// generation, so the pair is consistent even if another writer uploads in between.
func (b *GCSBlob) Get(ctx context.Context) ([]byte, int64, error) {
	gen, err := b.Generation(ctx)
	if err != nil {
		return nil, 0, err
	}
	resp, err := b.do(ctx, http.MethodGet, b.objectURL()+"?alt=media&generation="+strconv.FormatInt(gen, 10), nil, "")
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, 0, ErrNoObject
	default:
		return nil, 0, fmt.Errorf("object download: %s: %s", resp.Status, readErrorBody(resp.Body))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read object: %w", err)
	}
	return data, gen, nil
}

// Put implements Blob with a media upload and the ifGenerationMatch precondition.
func (b *GCSBlob) Put(ctx context.Context, data []byte, ifGenerationMatch int64) (int64, error) {
	q := url.Values{}
	q.Set("uploadType", "media")
	q.Set("name", b.Object)
	if ifGenerationMatch != NoGeneration {
		q.Set("ifGenerationMatch", strconv.FormatInt(ifGenerationMatch, 10))
	}
	u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?%s", b.storageURL(), url.PathEscape(b.Bucket), q.Encode())
	resp, err := b.do(ctx, http.MethodPost, u, bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusPreconditionFailed:
		return 0, ErrGenerationMismatch
	default:
		return 0, fmt.Errorf("object upload: %s: %s", resp.Status, readErrorBody(resp.Body))
	}
	return decodeGeneration(resp.Body)
}

// decodeGeneration reads {"generation":"123"} (GCS returns int64 as a string).
func decodeGeneration(r io.Reader) (int64, error) {
	var meta struct {
		Generation string `json:"generation"`
	}
	if err := json.NewDecoder(r).Decode(&meta); err != nil {
		return 0, fmt.Errorf("decode object metadata: %w", err)
	}
	gen, err := strconv.ParseInt(meta.Generation, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse generation %q: %w", meta.Generation, err)
	}
	return gen, nil
}

// readErrorBody returns a bounded excerpt of an error response for messages.
func readErrorBody(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	return string(bytes.TrimSpace(buf))
}
