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
