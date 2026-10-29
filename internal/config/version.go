package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var releaseVersionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+)([a-z]?))?$`)

// ReleaseVersion is an RMG-K release number such as 0.9.13b or v0.9.14.
// A missing revision letter sorts first, so 0.9.13 < 0.9.13a < 0.9.13b.
type ReleaseVersion struct {
	Major, Minor, Patch int
	Revision            byte // 0 when there is no letter
}

// ParseReleaseVersion parses a release tag. Anything else, including
// development builds (vdev-*), reports false.
func ParseReleaseVersion(raw string) (ReleaseVersion, bool) {
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return ReleaseVersion{}, false
	}
	var v ReleaseVersion
	var majorErr, minorErr, patchErr error
	v.Major, majorErr = strconv.Atoi(match[1])
	v.Minor, minorErr = strconv.Atoi(match[2])
	if match[3] != "" {
		v.Patch, patchErr = strconv.Atoi(match[3])
	}
	if majorErr != nil || minorErr != nil || patchErr != nil {
		return ReleaseVersion{}, false
	}
	if match[4] != "" {
		v.Revision = match[4][0]
	}
	return v, true
}

// Less reports whether v is an older release than o.
func (v ReleaseVersion) Less(o ReleaseVersion) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	return v.Revision < o.Revision
}

func (v ReleaseVersion) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Revision != 0 {
		s += string(v.Revision)
	}
	return s
}
