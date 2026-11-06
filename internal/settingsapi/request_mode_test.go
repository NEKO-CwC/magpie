package settingsapi

import (
	"encoding/json"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestRequestModeSettingsSave(t *testing.T) {
	home(t)
	if err := settings.Save(settings.Settings{RequestMode: settings.RequestManaged}); err != nil {
		t.Fatal(err)
	}
	for _, web := range []bool{false, true} {
		if _, _, err := Apply([]byte(`{"theme":"dark"}`), web); err != nil {
			t.Fatal(err)
		}
		if !settings.Load().ManagedRequests() {
			t.Fatal("an older page's save reset the request mode")
		}
	}
	for _, value := range []string{`"passthrough"`, `"managed"`, `null`, `""`} {
		body, err := Body(settings.Load(), "requestMode", json.RawMessage(value))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Apply(body, false); err != nil {
			t.Fatalf("requestMode=%s: %v", value, err)
		}
		if got := settings.Load().ManagedRequests(); got != (value == `"managed"`) {
			t.Fatalf("requestMode=%s: managed=%v", value, got)
		}
	}
	if _, _, err := Apply([]byte(`{"requestMode":"unknown"}`), false); err == nil {
		t.Fatal("the settings API accepted an unknown request mode")
	}
}
