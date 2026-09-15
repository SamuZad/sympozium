package charts

import "embed"

// Sympozium embeds the Sympozium Helm chart from charts/sympozium/.
//
//go:embed all:sympozium
var Sympozium embed.FS

// Ergoz embeds the vendored, packaged ergoz chart (accelerator power
// telemetry) from charts/ergoz/, pinned by config/ergoz/release.json.
//
//go:embed ergoz/*.tgz
var Ergoz embed.FS
