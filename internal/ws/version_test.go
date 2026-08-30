package ws

import "testing"

func TestSupportedClientVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"0.9.12", false},
		{"0.9.13", false},
		{"0.9.13a", false},
		{"0.9.13b", true},
		{"v0.9.13b", true},
		{"0.9.13c", true},
		{"0.9.14", true},
		{"0.9.14a", true},
		{"0.10.0", true},
		{"1.0.0", true},
		{"0.9", false},
		{"vdev-1", true},
		{"vdev-2373", true},
		{"vdev-2374", true},
		{"vdev-2375", true},
		{"vdev-custom", true},
		{"vdev-", true},
		{" vdev-test ", true},
		{"", false},
		{"smoke-test", false},
		{"9", false},
		{"0.9.13-beta", false},
		{"0.9.13bb", false},
		{"0.9.13B", false},
		{"VDEV-2374", false},
	}

	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			if got := isSupportedClientVersion(test.version); got != test.want {
				t.Fatalf("isSupportedClientVersion(%q) = %v, want %v", test.version, got, test.want)
			}
		})
	}
}
