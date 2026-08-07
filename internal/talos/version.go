package talos

import (
	"regexp"
	"sort"
	"strings"
)

// The Talos version appears in two places, neither of them a structured
// status field (the CRDs' status.versions / operatingSystemVersion are not
// populated by the operators today):
//
//   - Machine spec.os.imageID — the factory image URL the operator drives
//     provisioning/upgrades from, e.g.
//     https://factory.talos.dev/image/<schematic>/v1.13.7/nocloud-amd64.iso
//     This is the DECLARED (desired) version.
//   - Node status.nodeInfo.osImage — what the kubelet actually reports,
//     e.g. "Talos (v1.13.7)". This is the RUNTIME version.

var imageIDVersionRe = regexp.MustCompile(`/v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/`)
var osImageVersionRe = regexp.MustCompile(`(?i)talos.*?v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`)
var enforcementVersionRe = regexp.MustCompile(`(?i)all nodes run talos v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`)

// VersionFromImageID extracts the Talos version from a Machine's
// spec.os.imageID factory URL. Returns "" when no version is recognizable.
func VersionFromImageID(imageID string) string {
	m := imageIDVersionRe.FindStringSubmatch(imageID)
	if m == nil {
		return ""
	}
	return m[1]
}

// VersionFromOSImage extracts the Talos version from a Node's
// status.nodeInfo.osImage (e.g. "Talos (v1.13.7)"). Returns "" for
// non-Talos OS images or unrecognizable strings.
func VersionFromOSImage(osImage string) string {
	m := osImageVersionRe.FindStringSubmatch(osImage)
	if m == nil {
		return ""
	}
	return m[1]
}

// VersionFromEnforcement extracts the OPERATOR-VERIFIED runtime version from
// a TalosVersionEnforcement condition message ("All nodes run Talos v1.12.7").
// This is the most truthful mgmt-side source: unlike spec.os.imageID — which
// the operator bumps to the newest AVAILABLE image ahead of any actual
// upgrade (observed 112 days ahead of reality) — the enforcement condition
// states what the nodes actually run. Returns "" when the message has a
// different shape (e.g. mid-upgrade) or the condition is absent.
func VersionFromEnforcement(message string) string {
	m := enforcementVersionRe.FindStringSubmatch(message)
	if m == nil {
		return ""
	}
	return m[1]
}

// JoinVersions renders a set of versions compactly: "" for none, the single
// version when uniform, and a sorted comma-join when mixed (visible during
// rolling upgrades).
func JoinVersions(set map[string]struct{}) string {
	if len(set) == 0 {
		return ""
	}
	versions := make([]string, 0, len(set))
	for v := range set {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	return strings.Join(versions, ",")
}
