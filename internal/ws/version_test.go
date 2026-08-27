package ws

import "testing"

func TestSupportedClientVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"9.12", false},
		{"9.13", true},
		{"v9.13", true},
		{"9.13.0", true},
		{"9.14", true},
		{"10.0", true},
		{"vdev-2366", false},
		{"vdev-2367", true},
		{"vdev-2368", true},
		{"", false},
		{"smoke-test", false},
		{"9", false},
		{"9.13-beta", false},
		{"VDEV-2367", false},
	}

	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			if got := isSupportedClientVersion(test.version); got != test.want {
				t.Fatalf("isSupportedClientVersion(%q) = %v, want %v", test.version, got, test.want)
			}
		})
	}
}
