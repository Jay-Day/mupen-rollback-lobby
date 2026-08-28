package ws

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const (
	minimumReleaseMajor = 0
	minimumReleaseMinor = 9
	minimumReleasePatch = 13
	minimumDevBuild     = 2367

	minimumClientMessage = "Your RMG-K client is out of date. This lobby requires RMG-K 0.9.13 or vdev-2367 or newer."
)

var (
	errUnsupportedClientVersion = errors.New("unsupported RMG-K client version")
	releaseVersionPattern       = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?$`)
	devVersionPattern           = regexp.MustCompile(`^vdev-(\d+)$`)
)

// isSupportedClientVersion accepts the two version schemes emitted by RMG-K:
// release tags such as 0.9.13/v0.9.14 and monotonically numbered development
// builds such as vdev-2367. Unknown formats fail closed.
func isSupportedClientVersion(raw string) bool {
	version := strings.TrimSpace(raw)
	if match := devVersionPattern.FindStringSubmatch(version); match != nil {
		build, err := strconv.Atoi(match[1])
		return err == nil && build >= minimumDevBuild
	}

	match := releaseVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return false
	}
	major, majorErr := strconv.Atoi(match[1])
	minor, minorErr := strconv.Atoi(match[2])
	patch := 0
	var patchErr error
	if match[3] != "" {
		patch, patchErr = strconv.Atoi(match[3])
	}
	if majorErr != nil || minorErr != nil || patchErr != nil {
		return false
	}
	if major != minimumReleaseMajor {
		return major > minimumReleaseMajor
	}
	if minor != minimumReleaseMinor {
		return minor > minimumReleaseMinor
	}
	return patch >= minimumReleasePatch
}
