package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeArtifactStore struct {
	artifact  Artifact
	err       error
	downloads []string
}

func (s *fakeArtifactStore) Download(_ context.Context, blobName string) (Artifact, error) {
	s.downloads = append(s.downloads, blobName)
	if s.err != nil {
		return Artifact{}, s.err
	}
	return s.artifact, nil
}

func TestReadArtifactServesScopedBlob(t *testing.T) {
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("png-bytes"), ContentType: "image/png"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/01RUN/home.png", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "png-bytes" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if rec.Header().Get("content-type") != "image/png" {
		t.Fatalf("content-type=%q", rec.Header().Get("content-type"))
	}
	if rec.Header().Get("cache-control") != "public, max-age=300" {
		t.Fatalf("cache-control=%q", rec.Header().Get("cache-control"))
	}
	if len(store.downloads) != 1 || store.downloads[0] != "runs/glimmung/01RUN/home.png" {
		t.Fatalf("downloads=%#v", store.downloads)
	}
}

func TestReadArtifactRejectsUnscopedAndDotDotPaths(t *testing.T) {
	store := &fakeArtifactStore{}
	handler := NewWithDependencies(Settings{}, nil, nil, store)
	for _, path := range []string{
		"/v1/artifacts/private/secret.png",
		"/v1/artifacts/runs/glimmung/../secret.png",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if len(store.downloads) != 0 {
		t.Fatalf("downloads=%#v", store.downloads)
	}
}

func TestReadArtifactMapsStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "missing", err: ErrArtifactNotFound, want: http.StatusNotFound},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewWithDependencies(Settings{}, nil, nil, &fakeArtifactStore{err: tc.err})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/a.txt", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReadArtifactRequiresStore(t *testing.T) {
	handler := NewWithDependencies(Settings{}, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/a.txt", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
