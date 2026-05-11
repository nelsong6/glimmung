package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

var ErrArtifactNotFound = errors.New("artifact not found")

type Artifact struct {
	Body        []byte
	ContentType string
}

type ArtifactStore interface {
	Download(ctx context.Context, blobName string) (Artifact, error)
}

func readArtifact(store ArtifactStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "artifact store is not configured")
			return
		}
		blobName, ok := servingArtifactBlobName(r.PathValue("blob_path"))
		if !ok {
			writeProblem(w, http.StatusNotFound, "artifact not found")
			return
		}
		artifact, err := store.Download(r.Context(), blobName)
		switch {
		case errors.Is(err, ErrArtifactNotFound):
			writeProblem(w, http.StatusNotFound, "artifact not found")
			return
		case err != nil:
			writeProblem(w, http.StatusInternalServerError, "read artifact failed")
			return
		}
		contentType := artifact.ContentType
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("cache-control", "public, max-age=300")
		w.Header().Set("content-type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifact.Body)
	}
}

func rejectUnsafeArtifactPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.EscapedPath(), "/v1/artifacts/") {
			path := r.URL.Path
			if strings.Contains(path, "/../") || strings.Contains(path, "/./") ||
				strings.HasSuffix(path, "/..") || strings.HasSuffix(path, "/.") {
				writeProblem(w, http.StatusNotFound, "artifact not found")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func servingArtifactBlobName(blobPath string) (string, bool) {
	blobName := strings.Trim(blobPath, "/")
	if blobName == "" {
		return "", false
	}
	parts := strings.Split(blobName, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if !strings.HasPrefix(blobName, "runs/") &&
		!strings.HasPrefix(blobName, "issues/") &&
		!strings.HasPrefix(blobName, "reports/") {
		return "", false
	}
	return blobName, true
}
