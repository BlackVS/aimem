package adapter

import "testing"

// Rotating the checkpoint token through the legacy `aimem hub <url>
// <token>` path keeps the default hub's task credential, sync target and
// TLS choice.
func TestSaveHubKeepsOtherSettings(t *testing.T) {
	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{"home": {URL: "https://a", Token: "t1", TaskToken: "aimem_user_x", Sync: "aimem@a", Insecure: true}}, "home"); err != nil {
		t.Fatal(err)
	}
	if err := SaveHub(root, &HubConfig{URL: "https://a", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	hubs, def := LoadHubs(root)
	h := hubs[def]
	if def != "home" || h.Token != "t2" || h.TaskToken != "aimem_user_x" || h.Sync != "aimem@a" || !h.Insecure {
		t.Fatalf("settings lost on rotation: %+v", h)
	}
}
