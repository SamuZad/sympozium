// Package ergoz pins the ergoz release (accelerator power telemetry) that
// `sympozium install` deploys: the vendored chart under charts/ergoz must
// match this checksum before it is installed.
package ergoz

import (
	_ "embed"
	"encoding/json"
)

//go:embed release.json
var releaseJSON []byte

// Release is the pinned ergoz version and its packaged chart's identity.
type Release struct {
	Version     string `json:"version"`
	Chart       string `json:"chart"`
	ChartSHA256 string `json:"chartSHA256"`
}

// Pinned returns the release this build installs.
func Pinned() (Release, error) {
	var r Release
	if err := json.Unmarshal(releaseJSON, &r); err != nil {
		return Release{}, err
	}
	return r, nil
}
