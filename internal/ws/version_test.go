package ws

import (
	"testing"

	"github.com/jay-day/rmgk-lobby/internal/config"
)

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

	min := config.Defaults().MinClient()
	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			if got := isSupportedClientVersion(test.version, min); got != test.want {
				t.Fatalf("isSupportedClientVersion(%q) = %v, want %v", test.version, got, test.want)
			}
		})
	}
}

func TestSupportedClientVersionConfiguredMinimum(t *testing.T) {
	min, ok := config.ParseReleaseVersion("0.9.15")
	if !ok {
		t.Fatal("0.9.15 did not parse")
	}
	tests := []struct {
		version string
		want    bool
	}{
		{"0.9.13b", false},
		{"0.9.14z", false},
		{"0.9.15", true},
		{"v0.9.15a", true},
		{"0.10.0", true},
		{"vdev-2400", true},
	}
	for _, test := range tests {
		if got := isSupportedClientVersion(test.version, min); got != test.want {
			t.Errorf("isSupportedClientVersion(%q, 0.9.15) = %v, want %v", test.version, got, test.want)
		}
	}
}
