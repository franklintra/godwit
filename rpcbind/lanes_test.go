package rpcbind

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/rpc"
)

func methodList(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// The interactive list is pinned, and every name is a method the ui role
// can call: a renamed or removed method fails here instead of silently
// falling back to UIBackground.
func TestInteractiveMethodsPinned(t *testing.T) {
	pinned := methodList(t, "interactive_methods.txt")
	var got []string
	for m := range interactiveMethods {
		got = append(got, m)
	}
	slices.Sort(got)
	if !slices.Equal(got, pinned) {
		t.Fatalf("interactiveMethods = %v\nwant testdata/interactive_methods.txt = %v", got, pinned)
	}
	ui := slices.Concat(methodList(t, "full_methods.txt"), methodList(t, "ui_methods.txt"))
	for _, m := range pinned {
		if !slices.Contains(ui, m) {
			t.Errorf("%s is not a method of the ui role (full_methods.txt + ui_methods.txt)", m)
		}
	}
}

func TestLaneFor(t *testing.T) {
	for _, tc := range []struct {
		role, method, hint string
		want               store.Lane
	}{
		{rpc.RoleUI, "GetThread", "", store.Interactive},
		{rpc.RoleUI, "NewDraft", "", store.Interactive},
		{rpc.RoleUI, "SavedViews", "", store.UIBackground},
		{rpc.RoleUI, "ContactAvatar", "", store.UIBackground},
		{rpc.RoleUI, "StarredCounts", "", store.UIBackground},
		// A hint only lowers.
		{rpc.RoleUI, "GetThread", "ui-background", store.UIBackground},
		{rpc.RoleUI, "GetThread", "background", store.Background},
		{rpc.RoleUI, "SavedViews", "interactive", store.UIBackground},
		{rpc.RoleUI, "GetThread", "nonsense", store.Interactive},
		// The user's programs never take the interface's lanes.
		{rpc.RoleFull, "GetThread", "", store.Background},
		{rpc.RoleMCP, "GetThread", "interactive", store.Background},
	} {
		if got := laneFor(tc.role, tc.method, tc.hint); got != tc.want {
			t.Errorf("laneFor(%s, %s, %q) = %v, want %v", tc.role, tc.method, tc.hint, got, tc.want)
		}
	}
}
