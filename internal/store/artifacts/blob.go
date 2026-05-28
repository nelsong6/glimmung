package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/nelsong6/glimmung/internal/server"
)

type Store struct {
	client    *azblob.Client
	container string
}

func NewFromSettings(settings server.Settings) (*Store, error) {
	if strings.TrimSpace(settings.ArtifactsStorageAccount) == "" || strings.TrimSpace(settings.ArtifactsContainer) == "" {
		return nil, errors.New("ARTIFACTS_STORAGE_ACCOUNT and ARTIFACTS_CONTAINER are required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", strings.TrimSpace(settings.ArtifactsStorageAccount))
	client, err := azblob.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create blob client: %w", err)
	}
	return &Store{client: client, container: settings.ArtifactsContainer}, nil
}

// Upload writes the given bytes to blobName with the given content type. The
// Azure SDK's UploadBuffer overwrites an existing blob by default; callers are
// responsible for guaranteeing blobName uniqueness if that matters
// (slot_inspections rows are inserted with PK = inspection_id, which prevents
// collision in the inspection path). Returns the number of bytes written.
func (s *Store) Upload(ctx context.Context, blobName string, body []byte, contentType string) (int64, error) {
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	ct := contentType
	_, err := s.client.UploadBuffer(ctx, s.container, blobName, body, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &ct},
	})
	if err != nil {
		return 0, err
	}
	return int64(len(body)), nil
}

// Delete removes the blob at blobName. A blob that does not exist returns no
// error, so the lease-cleanup sweep is idempotent under partial failure.
func (s *Store) Delete(ctx context.Context, blobName string) error {
	_, err := s.client.DeleteBlob(ctx, s.container, blobName, nil)
	if err == nil {
		return nil
	}
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (s *Store) Download(ctx context.Context, blobName string) (server.Artifact, error) {
	response, err := s.client.DownloadStream(ctx, s.container, blobName, nil)
	if isStatus(err, http.StatusNotFound) {
		return server.Artifact{}, server.ErrArtifactNotFound
	}
	if err != nil {
		return server.Artifact{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return server.Artifact{}, err
	}
	contentType := "application/octet-stream"
	if response.ContentType != nil && strings.TrimSpace(*response.ContentType) != "" {
		contentType = *response.ContentType
	}
	return server.Artifact{Body: body, ContentType: contentType}, nil
}

func isStatus(err error, status int) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}
