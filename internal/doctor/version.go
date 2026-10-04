package doctor

import (
	"fmt"
	"regexp"
	"strconv"
)

type version struct {
	major, minor, patch int
	prerelease          string
}

var claudeMinimum = version{major: 2, minor: 1, patch: 277}
var codexMinimum = version{major: 0, minor: 156, patch: 1}
var versionPattern = regexp.MustCompile(`(?:^|\s)([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?(?:\s|$)`)

func parseVersion(output string) (version, bool) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return version{}, false
	}
	var numbers [3]int
	for i := range numbers {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return version{}, false
		}
		numbers[i] = n
	}
	return version{numbers[0], numbers[1], numbers[2], match[4]}, true
}

func (v version) less(minimum version) bool {
	for i, value := range [3]int{v.major, v.minor, v.patch} {
		limit := [3]int{minimum.major, minimum.minor, minimum.patch}[i]
		if value != limit {
			return value < limit
		}
	}
	return v.prerelease != ""
}

func (v version) String() string {
	value := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.prerelease != "" {
		value += "-" + v.prerelease
	}
	return value
}
