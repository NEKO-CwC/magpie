package davsync

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/backup"
	"github.com/yetone/magpie/internal/settings"
)

func TestRequestModeNeverUploaded(t *testing.T) {
	for _, mode := range []string{settings.RequestPassthrough, settings.RequestManaged} {
		for _, upload := range []bool{false, true} {
			name := mode
			if upload {
				name += "/explicit-upload"
			}
			t.Run(name, func(t *testing.T) {
				fake := &fakeDAV{files: map[string][]byte{}, etags: map[string]string{}, dirs: map[string]bool{"/dav": true}}
				srv := httptest.NewServer(fake)
				t.Cleanup(srv.Close)
				newComputer(t).use(t)
				cfg := Config{URL: srv.URL + "/dav/", User: "me", Password: "pw", Passphrase: "horse"}
				if err := Configure(cfg); err != nil {
					t.Fatal(err)
				}
				if err := settings.Save(settings.Settings{Theme: "dark", RequestMode: mode}); err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if upload {
					if err := Upload(ctx); err != nil {
						t.Fatal(err)
					}
				} else if err := Now(ctx); err != nil {
					t.Fatal(err)
				}
				check := func() {
					t.Helper()
					fake.mu.Lock()
					sealed := append([]byte(nil), fake.files["/dav/magpie/magpie.magpie-backup"]...)
					fake.mu.Unlock()
					b, err := backup.Open(sealed, cfg.Passphrase)
					if err != nil {
						t.Fatal(err)
					}
					if b.Settings == nil {
						t.Fatal("the upload has no settings")
					}
					j, _ := json.Marshal(b.Settings)
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(j, &fields); err != nil {
						t.Fatal(err)
					}
					if _, carried := fields["requestMode"]; carried {
						t.Fatalf("requestMode reached the uploaded settings: %s", j)
					}
					if s := settings.Load(); s.RequestMode != mode {
						t.Fatalf("preparing the upload changed the local mode to %q", s.RequestMode)
					}
				}
				check()
				fake.mu.Lock()
				firstPuts := fake.puts
				fake.mu.Unlock()
				// A later settings change takes the merge/upload path too.
				s := settings.Load()
				s.Theme = "light"
				if err := settings.Save(s); err != nil {
					t.Fatal(err)
				}
				if err := Now(ctx); err != nil {
					t.Fatal(err)
				}
				check()
				fake.mu.Lock()
				before := fake.puts
				fake.mu.Unlock()
				if before != firstPuts+1 {
					t.Fatal("the later settings change did not exercise a merged upload")
				}
				s = settings.Load()
				if mode == settings.RequestManaged {
					s.RequestMode = settings.RequestPassthrough
				} else {
					s.RequestMode = settings.RequestManaged
				}
				if err := settings.Save(s); err != nil {
					t.Fatal(err)
				}
				if err := Now(ctx); err != nil {
					t.Fatal(err)
				}
				fake.mu.Lock()
				after := fake.puts
				fake.mu.Unlock()
				if after != before {
					t.Fatal("changing only request mode triggered an upload")
				}
			})
		}
	}
}

func TestSyncIgnoresRemoteRequestMode(t *testing.T) {
	fake := &fakeDAV{files: map[string][]byte{}, etags: map[string]string{}, dirs: map[string]bool{"/dav": true}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	newComputer(t).use(t)
	cfg := Config{URL: srv.URL + "/dav/", User: "me", Password: "pw", Passphrase: "horse"}
	if err := Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{RequestMode: settings.RequestManaged}); err != nil {
		t.Fatal(err)
	}
	b := backup.Bundle{Version: 1, Settings: &settings.Settings{Theme: "dark", RequestMode: "unknown"}}
	sealed, err := backup.Seal(b, cfg.Passphrase)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.dirs["/dav/magpie"] = true
	fake.files["/dav/magpie/magpie.magpie-backup"] = sealed
	fake.etags["/dav/magpie/magpie.magpie-backup"] = `"remote"`
	fake.mu.Unlock()
	if err := Now(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := settings.Load(); !s.ManagedRequests() || s.Theme != "dark" {
		t.Fatalf("remote request mode changed this computer: mode=%q, theme=%q", s.RequestMode, s.Theme)
	}
	s := settings.Load()
	s.Theme, s.RequestMode = "light", settings.RequestPassthrough
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := settings.Load(); s.ManagedRequests() || s.Theme != "dark" {
		t.Fatalf("Restore changed this computer's mode: mode=%q, theme=%q", s.RequestMode, s.Theme)
	}
	s = settings.Load()
	s.RequestMode = settings.RequestManaged
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(); err != nil {
		t.Fatal(err)
	}
	if s := settings.Load(); !s.ManagedRequests() || s.Theme != "light" {
		t.Fatalf("Undo changed this computer's mode: mode=%q, theme=%q", s.RequestMode, s.Theme)
	}
}
