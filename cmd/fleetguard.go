package cmd

import (
	"fmt"

	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/settings"
)

// requireWholeFleet refuses a destructive operation when some configured
// availability zone could not be connected.
//
// kube.ConnectAll(allowPartial=true) drops unreachable zones with a warning on
// stderr and returns the survivors. That is right for read-only listings, but
// for a command that resolves a name and then destroys what it found it is
// not: a resource of the same name on a dropped zone can never be recognised
// as the ambiguity it is, so "the only match" is only the only match among the
// zones that happened to answer.
//
// Narrowing with -z is the escape hatch, and it is a real fix rather than a
// bypass: an explicit single zone makes the resolution unambiguous by
// definition, and ResolveAvailabilityZones then returns exactly that one zone,
// so this check passes for the right reason.
func requireWholeFleet(clients []*kube.Client, zones []settings.AvailabilityZone, what, name string) error {
	if len(clients) == len(zones) {
		return nil
	}
	return fmt.Errorf("refusing to act on %s %q: %d of %d configured availability zone(s) could not be connected "+
		"(warnings above), so a same-named %s there could not be ruled out — fix the connection, "+
		"or scope the command to one zone with -z",
		what, name, len(zones)-len(clients), len(zones), what)
}
