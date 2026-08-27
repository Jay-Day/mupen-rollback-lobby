package ws

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const (
	minimumReleaseMajor = 9
	minimumReleaseMinor = 13
	minimumDevBuild     = 2367

	minimumClientMessage = "Your RMG-K client is out of date. This lobby requires RMG-K 9.13 or vdev-2367 or newer."
)

var (
	errUnsupportedClientVersion = errors.New("unsupported RMG-K client version")
	releaseVersionPattern       = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?$`)
	devVersionPattern           = regexp.MustCompile(`^vdev-(\d+)$`)
)

// isSupportedClientVersion accepts the two version schemes emitted by RMG-K:
// release tags such as 9.13/v9.13.1 and monotonically numbered development
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
	if majorErr != nil || minorErr != nil {
		return false
	}
	return major > minimumReleaseMajor ||
		(major == minimumReleaseMajor && minor >= minimumReleaseMinor)
}
