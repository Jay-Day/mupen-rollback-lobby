package ws

import "testing"

func TestSupportedClientVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"0.9.12", false},
		{"0.9.13", true},
		{"v0.9.13", true},
		{"0.9.14", true},
		{"0.10.0", true},
		{"1.0.0", true},
		{"0.9", false},
		{"vdev-2366", false},
		{"vdev-2367", true},
		{"vdev-2368", true},
		{"", false},
		{"smoke-test", false},
		{"9", false},
		{"0.9.13-beta", false},
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
