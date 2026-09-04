// Package probe: parsing the pod log.
//
// Expected log, verbatim from runsc release-20260817.0:
//
//	runsc version release-20260817.0
//	spec: 1.2.1
//	---
//	535.129.03
//	535.183.06
//	...
//
// The parser is lenient about extra lines (a future runsc may print more
// version fields, or a warning) and strict about the two things the report
// depends on: a "runsc version" line and at least one driver version after
// the separator.
package probe

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	versionPrefix   = "runsc version "
	outputSeparator = "---"
)

// driverVersionRe matches NVIDIA driver versions: two to four dotted
// numeric components ("535.183.06", "550.54.15", "470.256.02").
var driverVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

// parseOutput extracts the runsc version and the sorted driver list.
func parseOutput(out string) (version string, drivers []string, err error) {
	lines := strings.Split(out, "\n")
	sep := -1
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if version == "" && strings.HasPrefix(l, versionPrefix) {
			version = strings.TrimSpace(strings.TrimPrefix(l, versionPrefix))
		}
		if sep < 0 && l == outputSeparator {
			sep = i
		}
	}
	if version == "" {
		return "", nil, fmt.Errorf("no %q line (is %s really runsc?)", strings.TrimSpace(versionPrefix), runscMountPath)
	}
	if sep < 0 {
		return "", nil, fmt.Errorf("no %q separator: 'runsc nvproxy list-supported-drivers' did not run (runsc too old?)", outputSeparator)
	}
	for _, raw := range lines[sep+1:] {
		if l := strings.TrimSpace(raw); driverVersionRe.MatchString(l) {
			drivers = append(drivers, l)
		}
	}
	if len(drivers) == 0 {
		return "", nil, fmt.Errorf("no driver versions after the separator")
	}
	sortVersions(drivers)
	return version, drivers, nil
}

// sortVersions orders dotted numeric versions numerically per component,
// so "535.129.03" < "535.183.06" < "550.54.15".
func sortVersions(v []string) {
	sort.SliceStable(v, func(i, j int) bool { return compareVersions(v[i], v[j]) < 0 })
}

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for k := 0; k < len(as) || k < len(bs); k++ {
		var x, y int
		if k < len(as) {
			x, _ = strconv.Atoi(as[k])
		}
		if k < len(bs) {
			y, _ = strconv.Atoi(bs[k])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
