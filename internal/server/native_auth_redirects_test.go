package server

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type fakeApplicationRedirectClient struct {
	app     ApplicationRedirectApp
	updated []string
	err     error
}

func (c *fakeApplicationRedirectClient) ReadApplicationRedirects(context.Context, ApplicationRedirectRef) (ApplicationRedirectApp, error) {
	if c.err != nil {
		return ApplicationRedirectApp{}, c.err
	}
	return c.app, nil
}

func (c *fakeApplicationRedirectClient) UpdateApplicationRedirects(_ context.Context, _ string, redirectURIs []string) error {
	c.updated = append([]string{}, redirectURIs...)
	return nil
}

func TestNativeAuthRedirectsReconcilesManagedSlotURIs(t *testing.T) {
	client := &fakeApplicationRedirectClient{app: ApplicationRedirectApp{
		ObjectID: "app-object",
		ClientID: "client-id",
		RedirectURIs: []string{
			"https://tank.romaine.life/",
			"https://manual.example.com/callback",
			"https://tank-slot-1.tank.dev.romaine.life/",
			"https://tank-slot-4.tank.dev.romaine.life/",
		},
	}}
	service := NativeAuthRedirectService{
		Client: client,
		Now:    func() time.Time { return time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC) },
	}
	project := Project{
		Name: "tank",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"record_base": "tank.dev.romaine.life",
				"slot_prefix": "tank-slot",
				"count":       float64(3),
			},
			"native_auth_redirects": map[string]any{
				"enabled":                  true,
				"application_object_id":    "app-object",
				"production_redirect_uris": []any{"https://tank.romaine.life/"},
				"extra_redirect_uris":      []any{"https://extra.example.com/"},
			},
		},
	}

	status, err := service.ReconcileNativeAuthRedirects(context.Background(), project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{
		"https://extra.example.com/",
		"https://manual.example.com/callback",
		"https://tank-slot-1.tank.dev.romaine.life/",
		"https://tank-slot-2.tank.dev.romaine.life/",
		"https://tank-slot-3.tank.dev.romaine.life/",
		"https://tank.romaine.life/",
	}
	if !reflect.DeepEqual(client.updated, want) {
		t.Fatalf("updated=%#v want %#v", client.updated, want)
	}
	if status.State != NativeAuthRedirectStatusOK {
		t.Fatalf("status=%#v", status)
	}
	assertStringSlice(t, status.AddedRedirectURIs, []string{
		"https://extra.example.com/",
		"https://tank-slot-2.tank.dev.romaine.life/",
		"https://tank-slot-3.tank.dev.romaine.life/",
	})
	assertStringSlice(t, status.RemovedRedirectURIs, []string{"https://tank-slot-4.tank.dev.romaine.life/"})
}

func TestNativeAuthRedirectsDoesNotRemoveManualLookalikes(t *testing.T) {
	cfg := nativeAuthRedirectConfig{
		RecordBase: "tank.dev.romaine.life",
		SlotPrefix: "tank-slot",
		Count:      1,
	}
	desired, _, removed := reconcileRedirectURISet([]string{
		"https://tank-slot-2.tank.dev.romaine.life/",
		"https://tank-slot-2.tank.dev.romaine.life/auth/callback",
		"https://tank-slot-extra.tank.dev.romaine.life/",
		"https://tank-slot-2.other.example/",
	}, cfg)

	assertStringSlice(t, removed, []string{"https://tank-slot-2.tank.dev.romaine.life/"})
	assertContains(t, desired, "https://tank-slot-2.tank.dev.romaine.life/auth/callback")
	assertContains(t, desired, "https://tank-slot-extra.tank.dev.romaine.life/")
	assertContains(t, desired, "https://tank-slot-2.other.example/")
}

func TestNativeAuthRedirectsSkippedWhenDisabled(t *testing.T) {
	service := NativeAuthRedirectService{Client: &fakeApplicationRedirectClient{}}
	status, err := service.ReconcileNativeAuthRedirects(context.Background(), Project{Metadata: map[string]any{}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != "" {
		t.Fatalf("status=%#v", status)
	}
}

func TestNativeAuthRedirectsAcceptsLegacyStandbyEntraMetadata(t *testing.T) {
	cfg, ok, err := nativeAuthRedirectConfigFromProject(Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"native_standby_dns": map[string]any{
				"record_base": "tank.dev.romaine.life",
				"slot_prefix": "tank-operator-slot",
				"count":       float64(10),
			},
			"native_standby_entra_redirects": map[string]any{
				"enabled":            true,
				"application_app_id": "c189a2aa-adf8-466b-a699-156cbfd9810c",
			},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v cfg=%#v", ok, err, cfg)
	}
	if cfg.ApplicationClientID != "c189a2aa-adf8-466b-a699-156cbfd9810c" {
		t.Fatalf("cfg=%#v", cfg)
	}
	if got := desiredManagedRedirectURIs(cfg); len(got) != 10 || got[9] != "https://tank-operator-slot-10.tank.dev.romaine.life/" {
		t.Fatalf("managed=%#v", got)
	}
}

func assertStringSlice(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("%q not in %#v", want, values)
}
