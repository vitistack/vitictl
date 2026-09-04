package decommission

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// writeStub creates a fake viti-nhn binary that records its argv and exits
// with the given code.
func writeStub(t *testing.T, exitCode int) (bin, argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub-script test is unix-only")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "viti-nhn")
	argvFile = filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$@\" > " + argvFile + "\necho purged from ROR: clusters=1\nexit " +
		map[int]string{0: "0", 1: "1"}[exitCode] + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvFile
}

func TestNHNBinaryEnvOverride(t *testing.T) {
	bin, _ := writeStub(t, 0)
	t.Setenv(EnvNHNBinary, bin)
	got, err := nhnBinary()
	if err != nil || got != bin {
		t.Fatalf("nhnBinary() = %q, %v — want the env override", got, err)
	}
}

func TestNHNBinaryMissing(t *testing.T) {
	t.Setenv(EnvNHNBinary, "")
	t.Setenv("PATH", t.TempDir()) // empty PATH: nothing to find
	if _, err := nhnBinary(); err == nil {
		t.Fatal("expected lookup error with viti-nhn absent")
	}
}

func TestRunRORPurgeDelegatesWithForce(t *testing.T) {
	bin, argvFile := writeStub(t, 0)
	p := &rorPurge{clusterID: "t-x-abcd"}
	p.exec(t.Context(), bin, true)

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("stub was not executed: %v", err)
	}
	got := string(raw)
	want := "ror purge t-x-abcd --yes --wait --force\n"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if !p.ok {
		t.Error("purge verdict should be ok on exit 0")
	}
	if len(p.log) == 0 {
		t.Error("plugin output should be captured into the log")
	}
}

func TestRunRORPurgeFailurePropagates(t *testing.T) {
	bin, _ := writeStub(t, 1)
	p := &rorPurge{clusterID: "t-x-abcd"}
	p.exec(t.Context(), bin, false)
	if p.ok {
		t.Error("purge verdict must not be ok on non-zero exit")
	}
}

func TestStartRORPurgeNamespaceLookupErrorBlocksVerdict(t *testing.T) {
	sch := testScheme(t, false, true)
	base := fakeClient(sch)
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		Get: func(_ context.Context, _ ctrlclient.WithWatch, key ctrlclient.ObjectKey, _ ctrlclient.Object, _ ...ctrlclient.GetOption) error {
			if key.Name == "nhn-ror" {
				return errBoom
			}
			return nil
		},
	})
	r, out := newTestRunner(t, nil, guest)

	r.startRORPurge(t.Context())

	if !r.failed {
		t.Fatal("an unreadable nhn-ror namespace must block the preclean verdict")
	}
	if !strings.Contains(out.String(), "ROR purge cannot be verified") {
		t.Errorf("output = %q, want the blocking lookup failure", out.String())
	}
}

func TestStartRORPurgeMissingSecretBlocksVerdict(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "nhn-ror"}})
	r, out := newTestRunner(t, nil, guest)

	r.startRORPurge(t.Context())

	if !r.failed {
		t.Fatal("a missing ROR identity secret must block the preclean verdict")
	}
	if !strings.Contains(out.String(), "could not read secret nhn-ror/ror-apikey") {
		t.Errorf("output = %q, want the blocking secret failure", out.String())
	}
}
