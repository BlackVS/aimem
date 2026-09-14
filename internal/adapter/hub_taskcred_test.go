package adapter

import "testing"

// Re-registering the same host keeps the task credential and sync target
// that were not restated; a new host inherits nothing; the TLS downgrade
// is never inherited.
func TestHubConfigOver(t *testing.T) {
	prev := &HubConfig{URL: "https://a", Token: "t1", TaskToken: "aimem_user_x", Sync: "aimem@a", Insecure: true}
	cases := []struct {
		name string
		next *HubConfig
		want HubConfig
	}{
		{"same host rotates the token", &HubConfig{URL: "https://a", Token: "t2"}, HubConfig{URL: "https://a", Token: "t2", TaskToken: "aimem_user_x", Sync: "aimem@a"}},
		{"same host restates insecure", &HubConfig{URL: "https://a", Token: "t2", Insecure: true}, HubConfig{URL: "https://a", Token: "t2", TaskToken: "aimem_user_x", Sync: "aimem@a", Insecure: true}},
		{"same host restates sync", &HubConfig{URL: "https://a", Token: "t2", Sync: "aimem@b"}, HubConfig{URL: "https://a", Token: "t2", TaskToken: "aimem_user_x", Sync: "aimem@b"}},
		{"new host inherits nothing", &HubConfig{URL: "https://b", Token: "t2"}, HubConfig{URL: "https://b", Token: "t2"}},
		{"no previous", nil, HubConfig{}},
	}
	for _, c := range cases {
		var got *HubConfig
		if c.next == nil {
			got = (&HubConfig{}).Over(nil)
		} else {
			got = c.next.Over(prev)
		}
		if *got != c.want {
			t.Fatalf("%s: got %+v want %+v", c.name, *got, c.want)
		}
	}
	// Through the legacy default-hub path on disk.
	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{"home": prev}, "home"); err != nil {
		t.Fatal(err)
	}
	if err := SaveHub(root, &HubConfig{URL: "https://a", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	hubs, def := LoadHubs(root)
	if h := hubs[def]; def != "home" || h.Token != "t2" || h.TaskToken != "aimem_user_x" || h.Insecure {
		t.Fatalf("rotation on disk: %+v", h)
	}
}
