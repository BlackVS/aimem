package main

import "testing"

// Project targets carry the checkout as their Root, so Install confines
// them; user-level targets do not.
func TestSkillsTargetsConfineOnlyProjectDirectories(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	for _, user := range []bool{false, true} {
		ts, err := skillsTargets(dir, home, user, "claude,codex")
		if err != nil || len(ts) != 2 {
			t.Fatalf("user=%v: %+v %v", user, ts, err)
		}
		for _, tg := range ts {
			if want := map[bool]string{false: dir, true: ""}[user]; tg.Root != want {
				t.Fatalf("user=%v: %s has Root %q, want %q", user, tg.Client, tg.Root, want)
			}
		}
	}
}
