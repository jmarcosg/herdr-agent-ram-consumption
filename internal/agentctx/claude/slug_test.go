package claude

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	longASCII := "-home-ana-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-folder-name-very-long-fold-3mvvc4"
	tests := []struct{ name, cwd, want string }{
		{"plain", "/Users/jmarcosg/dev/minimalart", "-Users-jmarcosg-dev-minimalart"},
		{"non ascii", "/tmp/añ b", "-tmp-a--b"},
		{"dots", "/home/a/.cornercase/worktrees/shop/feat-x", "-home-a--cornercase-worktrees-shop-feat-x"},
		{"emoji is two units", "/home/ana/código/🦀 shop", "-home-ana-c-digo----shop"},
		{"long ascii", "/home/ana/" + strings.Repeat("very-long-folder-name/", 10) + "repo", longASCII},
		{"long counts utf16 units", "/home/ana/" + strings.Repeat("ñ", 195) + "/x", "-home-ana-" + strings.Repeat("-", 190) + "-ck6jya"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.cwd); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestSlugLongBounds(t *testing.T) {
	got := Slug("/" + strings.Repeat("a", 249))
	if len(got) > 200+1+7 || len(got) <= 200 {
		t.Errorf("len = %d", len(got))
	}
}
