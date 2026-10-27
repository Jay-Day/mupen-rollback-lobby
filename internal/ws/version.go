package ws

import (
	"errors"
	"strings"

	"github.com/jay-day/rmgk-lobby/internal/config"
)

var errUnsupportedClientVersion = errors.New("unsupported RMG-K client version")

// isSupportedClientVersion accepts the two version schemes emitted by RMG-K:
// release tags such as 0.9.13b/v0.9.14 at or above min, and development builds
// beginning with vdev-. Unknown non-development formats fail closed.
func isSupportedClientVersion(raw string, min config.ReleaseVersion) bool {
	version := strings.TrimSpace(raw)
	if strings.HasPrefix(version, "vdev-") {
		return true
	}
	v, ok := config.ParseReleaseVersion(version)
	return ok && !v.Less(min)
}
