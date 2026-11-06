package settings

import "testing"

func TestRequestModeDefaultsAndPersists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if s := Load(); s.RequestMode != "" || s.ManagedRequests() {
		t.Fatalf("a missing setting must use passthrough: %q", s.RequestMode)
	}
	for _, mode := range []string{RequestManaged, RequestPassthrough, ""} {
		if err := Save(Settings{RequestMode: mode}); err != nil {
			t.Fatal(err)
		}
		if s := Load(); s.RequestMode != mode || s.ManagedRequests() != (mode == RequestManaged) {
			t.Fatalf("saved %q, loaded %q (managed %v)", mode, s.RequestMode, s.ManagedRequests())
		}
	}
	if err := Save(Settings{RequestMode: RequestManaged}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Settings{RequestMode: "unknown"}); err == nil {
		t.Fatal("an unknown request mode was accepted")
	}
	if !Load().ManagedRequests() {
		t.Fatal("a rejected save changed the request mode")
	}
}
