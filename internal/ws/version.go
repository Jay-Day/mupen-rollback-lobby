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
	// A missing revision sorts before "a", so 0.9.13 < 0.9.13a < 0.9.13b.
	minimumReleaseRevision = 'b'

	minimumClientMessage = "Your RMG-K client is out of date. This lobby requires RMG-K 0.9.13b or any vdev build."
)

var (
	errUnsupportedClientVersion = errors.New("unsupported RMG-K client version")
	releaseVersionPattern       = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+)([a-z]?))?$`)
)

// isSupportedClientVersion accepts the two version schemes emitted by RMG-K:
// release tags such as 0.9.13b/v0.9.14 and development builds beginning with
// vdev-. Unknown non-development formats fail closed.
func isSupportedClientVersion(raw string) bool {
	version := strings.TrimSpace(raw)
	if strings.HasPrefix(version, "vdev-") {
		return true
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
	revision := byte(0)
	if match[4] != "" {
		revision = match[4][0]
	}
	if major != minimumReleaseMajor {
		return major > minimumReleaseMajor
	}
	if minor != minimumReleaseMinor {
		return minor > minimumReleaseMinor
	}
	if patch != minimumReleasePatch {
		return patch > minimumReleasePatch
	}
	return revision >= minimumReleaseRevision
}
