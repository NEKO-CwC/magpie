package backup

import (
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestBackupKeepsRequestModeForLocalRecovery(t *testing.T) {
	home(t)
	if err := settings.Save(settings.Settings{RequestMode: settings.RequestManaged}); err != nil {
		t.Fatal(err)
	}
	b, err := Collect(false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if b.Settings == nil || b.Settings.RequestMode != settings.RequestManaged {
		t.Fatal("collecting a local backup discarded its request mode")
	}
}

func TestRestoreKeepsRequestMode(t *testing.T) {
	for _, local := range []string{"", settings.RequestPassthrough, settings.RequestManaged} {
		t.Run("local="+local, func(t *testing.T) {
			home(t)
			if err := settings.Save(settings.Settings{Theme: "dark", RequestMode: local}); err != nil {
				t.Fatal(err)
			}
			for _, remote := range []string{"", settings.RequestPassthrough, settings.RequestManaged, "unknown"} {
				b := Bundle{Version: 1, Settings: &settings.Settings{Theme: "light", RequestMode: remote}}
				if _, err := Restore(b, Parts{Settings: true}); err != nil {
					t.Fatal(err)
				}
				if s := settings.Load(); s.RequestMode != local || s.Theme != "light" {
					t.Fatalf("remote %q changed local %q: mode=%q theme=%q", remote, local, s.RequestMode, s.Theme)
				}
			}
		})
	}
}
