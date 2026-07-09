package webui

import "testing"

// The placeholder detection is tested as pure logic against literal content, so
// it passes regardless of whether the embedded dist/ currently holds the
// committed placeholder or a real Vite build (it flips between the two across
// builds, and a real bundle must be present to run the app / E2E).
func TestIsPlaceholderContent(t *testing.T) {
	cases := []struct {
		name  string
		index string
		want  bool
	}{
		{"committed placeholder", `<!doctype html><title>juicebox-spa-placeholder</title>`, true},
		{"real vite build", `<!doctype html><script type="module" src="/assets/index-abc123.js"></script>`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlaceholderContent([]byte(tc.index)); got != tc.want {
				t.Errorf("isPlaceholderContent(%q) = %v, want %v", tc.index, got, tc.want)
			}
		})
	}
}
