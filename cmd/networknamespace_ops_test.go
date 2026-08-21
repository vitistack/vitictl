package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/settings"
)

// TestConfirmTypedName exercises the last guard standing between the operator
// and an irreversible NAM teardown (VLAN and prefixes released in an external
// system). The prompt is shared by 'nn delete' and 'kc delete'.
func TestConfirmTypedName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		target  string
		wantErr string
	}{
		{"the exact name confirms", "team-a-x1\n", "team-a-x1", ""},
		{"surrounding whitespace is tolerated", "  team-a-x1  \n", "team-a-x1", ""},
		{"a different name aborts", "team-a-x2\n", "team-a-x1", "did not match"},
		{"a bare newline aborts", "\n", "team-a-x1", "did not match"},
		{"a prefix of the name aborts", "team-a\n", "team-a-x1", "did not match"},
		{"case must match", "TEAM-A-X1\n", "team-a-x1", "did not match"},
		// A truncated answer is not consent: input that ends at EOF without a
		// newline is refused rather than accepted, so a half-written pipe can
		// never authorise the deletion. --yes is the scripted path.
		{"EOF with no newline is refused", "team-a-x1", "team-a-x1", "reading confirmation"},
		{"no input at all is refused", "", "team-a-x1", "reading confirmation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, out := stubCmd(tt.input)
			err := confirmTypedName(cmd, "networknamespace", "deletion", tt.target)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("confirmTypedName(%q) = %v, want nil", tt.input, err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("confirmTypedName(%q) = nil, want an error containing %q", tt.input, tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("confirmTypedName(%q) = %v, want it to contain %q", tt.input, err, tt.wantErr)
			}
			if !strings.Contains(out.String(), "networknamespace name") {
				t.Errorf("prompt = %q, want it to name what must be typed", out.String())
			}
		})
	}
}

// TestConfirmTypedNameKeepsBothPrompts pins the wording of the two call sites
// after they were collapsed onto one implementation.
func TestConfirmTypedNameKeepsBothPrompts(t *testing.T) {
	cmd, out := stubCmd("c1\n")
	if err := confirmKcDelete(cmd, "c1"); err != nil {
		t.Fatalf("confirmKcDelete: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Type the cluster name to confirm decommission:") {
		t.Errorf("kc prompt = %q", got)
	}

	cmd, out = stubCmd("team-a-x1\n")
	if err := confirmTypedName(cmd, "networknamespace", "deletion", "team-a-x1"); err != nil {
		t.Fatalf("confirmTypedName: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Type the networknamespace name to confirm deletion:") {
		t.Errorf("nn prompt = %q", got)
	}
}

// TestAuditSummary pins the coverage accounting of 'nn orphans'.
//
// The property under test is honesty about what was NOT looked at. The
// denominator is the number of CONFIGURED availability zones: zones that
// failed to connect are dropped by ConnectAll before any query runs, and
// counting only the survivors used to render "2/2 availability zone(s)
// audited" for a fleet of five — a clean-sweep verdict covering three zones
// nobody queried. A non-empty warning is what makes the command exit
// non-zero, so partial coverage is detectable by automation.
func TestAuditSummary(t *testing.T) {
	tests := []struct {
		name                                  string
		orphans, blocked, audited, configured int
		wantLine, wantWarn                    string
		wantNotInLine                         string
		wantNoLine, wantNoWarn                bool
	}{
		{
			name:    "full coverage, nothing found",
			orphans: 0, audited: 3, configured: 3,
			wantLine:   "🧹 no orphaned networknamespaces found (3/3 availability zone(s) audited)",
			wantNoWarn: true,
		},
		{
			name:    "full coverage with orphans",
			orphans: 2, audited: 3, configured: 3,
			wantLine:   "2 orphan(s), 3/3 availability zone(s) audited",
			wantNoWarn: true,
		},
		{
			// The regression: 3 of 5 zones never connected, so len(clients)
			// was 2 and the audit called itself complete.
			name:    "zones that never connected still count against coverage",
			orphans: 0, audited: 2, configured: 5,
			wantLine: "🧹 no orphaned networknamespaces found (2/5 availability zone(s) audited)",
			wantWarn: "3 of 5 availability zone(s) could not be audited",
		},
		{
			name:    "connected zones that timed out are skipped too",
			orphans: 1, audited: 1, configured: 3,
			wantLine: "1 orphan(s), 1/3 availability zone(s) audited",
			wantWarn: "2 of 3 availability zone(s) could not be audited",
		},
		{
			// Zero coverage must never render a broom: nothing was verified.
			name:    "no zone audited at all is not a clean sweep",
			orphans: 0, audited: 0, configured: 4,
			wantNoLine: true,
			wantWarn:   "NO availability zone could be audited (0/4)",
		},
		{
			name:    "single configured zone, audited",
			orphans: 0, audited: 1, configured: 1,
			wantLine:   "🧹 no orphaned networknamespaces found (1/1 availability zone(s) audited)",
			wantNoWarn: true,
		},
		{
			// A listed orphan that delete would refuse can now only mean
			// unreadable evidence, so the footnote points at investigating it
			// rather than at deleting.
			name:    "an undeletable orphan is called out, not left to the reader",
			orphans: 3, blocked: 1, audited: 3, configured: 3,
			wantLine:   "1 of them cannot be deleted",
			wantNoWarn: true,
		},
		{
			// Silent in the normal case: "0 of them are blocked" on every run
			// is exactly the noise this command sheds elsewhere.
			name:    "nothing blocked says nothing",
			orphans: 3, blocked: 0, audited: 3, configured: 3,
			wantLine:      "3 orphan(s), 3/3 availability zone(s) audited",
			wantNotInLine: "cannot be deleted",
			wantNoWarn:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, warning := auditSummary(tt.orphans, tt.blocked, tt.audited, tt.configured)
			if tt.wantNoLine && line != "" {
				t.Errorf("line = %q, want none", line)
			}
			if tt.wantLine != "" && !strings.Contains(line, tt.wantLine) {
				t.Errorf("line = %q, want it to contain %q", line, tt.wantLine)
			}
			if tt.wantNotInLine != "" && strings.Contains(line, tt.wantNotInLine) {
				t.Errorf("line = %q, want it NOT to contain %q", line, tt.wantNotInLine)
			}
			if tt.wantNoWarn && warning != "" {
				t.Errorf("warning = %q, want none for complete coverage", warning)
			}
			if tt.wantWarn != "" && !strings.Contains(warning, tt.wantWarn) {
				t.Errorf("warning = %q, want it to contain %q", warning, tt.wantWarn)
			}
			// Whenever coverage is short, the command must be able to fail.
			if tt.audited < tt.configured && warning == "" {
				t.Errorf("audited %d of %d configured but no warning — partial coverage would exit 0",
					tt.audited, tt.configured)
			}
		})
	}
}

func nnScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

// zoneClient builds a *kube.Client backed by a fake API. When listErr is
// non-nil every NetworkNamespace List against the zone fails with it.
func zoneClient(t *testing.T, azName string, listErr error, netnsNames ...string) *kube.Client {
	t.Helper()
	objs := make([]ctrlclient.Object, 0, len(netnsNames))
	for _, n := range netnsNames {
		objs = append(objs, &vitiv1alpha1.NetworkNamespace{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: n},
		})
	}
	b := fake.NewClientBuilder().WithScheme(nnScheme(t)).WithObjects(objs...)
	if listErr != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				if _, ok := list.(*vitiv1alpha1.NetworkNamespaceList); ok {
					return listErr
				}
				return cl.List(ctx, list, opts...)
			},
		})
	}
	return &kube.Client{AZ: settings.AvailabilityZone{Name: azName}, Ctrl: b.Build()}
}

// TestFindNetNSAcrossAZsAbortsOnUnsearchableZone is the load-bearing claim of
// findNetNSAcrossAZs' doc comment: a zone that cannot be queried aborts the
// search instead of narrowing it. A single match found in the zones that DID
// answer is not "the only match" — the unsearched zone may hold a
// same-named netns, and resolving to the wrong object here means tearing down
// another team's VLAN and prefixes.
func TestFindNetNSAcrossAZsAbortsOnUnsearchableZone(t *testing.T) {
	boom := errors.New("conversion webhook unavailable")

	tests := []struct {
		name    string
		clients []*kube.Client
	}{
		{
			name: "the unsearchable zone comes first",
			clients: []*kube.Client{
				zoneClient(t, "pos1", boom),
				zoneClient(t, "osl2", nil, "team-a-x1"),
			},
		},
		{
			// The dangerous ordering: a match is already in hand when the
			// failure is seen, so returning it would look reasonable.
			name: "the unsearchable zone comes after the only match",
			clients: []*kube.Client{
				zoneClient(t, "osl2", nil, "team-a-x1"),
				zoneClient(t, "pos1", boom),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hit, err := findNetNSAcrossAZs(context.Background(), tt.clients, "team-a-x1", "", 5*time.Second)
			if err == nil {
				t.Fatal("want an error: the fleet was only partially searched")
			}
			if hit != nil {
				t.Fatalf("want no hit, got %s/%s — a partially-searched fleet must never resolve a target",
					hit.client.AZ.Name, hit.nn.Name)
			}
			if !strings.Contains(err.Error(), "pos1") {
				t.Errorf("error must name the zone that could not be searched, got: %v", err)
			}
			if !strings.Contains(err.Error(), "unambiguously") {
				t.Errorf("error must explain that the name cannot be resolved unambiguously, got: %v", err)
			}
			if !errors.Is(err, boom) {
				t.Errorf("error must wrap the underlying cause, got: %v", err)
			}
		})
	}
}

// TestFindNetNSAcrossAZsResolvesAndDetectsAmbiguity is the control: with every
// zone answering, exactly one match resolves and two matches refuse.
func TestFindNetNSAcrossAZsResolvesAndDetectsAmbiguity(t *testing.T) {
	ctx := context.Background()

	single := []*kube.Client{
		zoneClient(t, "pos1", nil, "team-a-other"),
		zoneClient(t, "osl2", nil, "team-a-x1"),
	}
	hit, err := findNetNSAcrossAZs(ctx, single, "team-a-x1", "", 5*time.Second)
	if err != nil {
		t.Fatalf("findNetNSAcrossAZs: %v", err)
	}
	if hit.client.AZ.Name != "osl2" || hit.nn.Name != "team-a-x1" {
		t.Fatalf("resolved %s/%s, want osl2/team-a-x1", hit.client.AZ.Name, hit.nn.Name)
	}

	ambiguous := []*kube.Client{
		zoneClient(t, "pos1", nil, "team-a-x1"),
		zoneClient(t, "osl2", nil, "team-a-x1"),
	}
	hit, err = findNetNSAcrossAZs(ctx, ambiguous, "team-a-x1", "", 5*time.Second)
	if err == nil {
		t.Fatal("want an ambiguity error when the name exists on two zones")
	}
	if hit != nil {
		t.Fatal("an ambiguous name must never resolve to a target")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v, want it to say the name is ambiguous", err)
	}
}
