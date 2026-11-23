package e2e

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Offline isolation contract for the client-go E2E suite.
//
// Choice documented per the campaign's offline-verification rule: a behavioral
// namespace/UID-precondition test needs a live cluster, which offline work must
// not mutate. So the invariants are asserted at the source level (a grep
// contract over e2e_test.go) plus a behavioral test of the pure ownership gate
// runOwnedNamespaceName, which needs no cluster. The live run-owned namespace
// and create-UID cleanup are exercised when E2E_RUN=1 against a real cluster.

func readE2ESource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("e2e_test.go")
	if err != nil {
		t.Fatalf("read e2e_test.go: %v", err)
	}
	return string(b)
}

// funcBody returns the source of a top-level function, up to its closing brace
// at column zero (gofmt layout). Inner closures are indented and do not match.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found in e2e_test.go", name)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("function %s body not terminated", name)
	}
	return rest[:end]
}

func TestE2EIsolationContract(t *testing.T) {
	src := readE2ESource(t)

	// No shared/default namespace literal remains in the live source.
	if strings.Contains(src, `"default"`) {
		t.Errorf("e2e_test.go still contains a \"default\" namespace literal")
	}
	// No hardcoded namespace argument in namespaced client calls; every object
	// must live in the run-owned namespace variable.
	if regexp.MustCompile(`(Pods|PersistentVolumeClaims)\(\s*"`).MatchString(src) {
		t.Errorf("a namespaced client call uses a hardcoded namespace literal")
	}
	// No precondition-free delete: every DeleteOptions must carry preconditions.
	if strings.Contains(src, "metav1.DeleteOptions{}") {
		t.Errorf("a delete uses empty DeleteOptions without UID preconditions")
	}
	// UID preconditions are present on the cleanup paths (PVC, Pod, SC, NS, and
	// the NFS static PV when present).
	if n := strings.Count(src, "Preconditions: &metav1.Preconditions{UID:"); n < 4 {
		t.Errorf("expected UID preconditions on cleanup deletes, found %d", n)
	}
	// A run-owned namespace is created with the run prefix, not adopted.
	if !strings.Contains(src, `namespacePrefix = "quartet-e2e-"`) {
		t.Errorf("run-owned namespace prefix missing")
	}
	if !strings.Contains(src, "GenerateName: namespacePrefix") {
		t.Errorf("namespace is not created with the GenerateName run prefix")
	}
	if !strings.Contains(src, "Namespaces().Create") {
		t.Errorf("run-owned namespace is not created")
	}
	// Namespace cleanup carries the create-UID precondition and fails the test
	// on any error/residue rather than silently leaving it behind.
	nsCleanup := funcBody(t, src, "deleteRunNamespace")
	if !strings.Contains(nsCleanup, "Preconditions: &metav1.Preconditions{UID:") {
		t.Errorf("namespace cleanup lacks the UID precondition")
	}
	if !strings.Contains(nsCleanup, "t.Errorf") {
		t.Errorf("namespace cleanup does not fail the test on error")
	}
	// Object cleanup helpers carry create-UID preconditions.
	for _, fn := range []string{"deletePVC", "deletePod", "deleteStorageClass"} {
		body := funcBody(t, src, fn)
		if !strings.Contains(body, "Preconditions: &metav1.Preconditions{UID:") {
			t.Errorf("%s lacks the UID precondition", fn)
		}
	}
}

func TestRunOwnedNamespaceName(t *testing.T) {
	reject := []string{
		"", "default", "kube-system", "kube-public", "kube-node-lease",
		"ngxstorage-nfs", "other-ns",
		"quartet-e2e-",                           // bare prefix, no unique suffix
		"quartet-e2e-ABC",                        // uppercase is not a DNS-1123 label
		"quartet-e2e-a_b",                        // underscore is not a DNS-1123 label
		"quartet-e2e-" + strings.Repeat("a", 60), // exceeds 63 chars
	}
	for _, name := range reject {
		if runOwnedNamespaceName(name) {
			t.Errorf("runOwnedNamespaceName(%q) = true, want false (not run-owned)", name)
		}
	}
	accept := []string{
		"quartet-e2e-run1",
		"quartet-e2e-abc123",
		"quartet-e2e-" + strings.Repeat("a", 40),
	}
	for _, name := range accept {
		if !runOwnedNamespaceName(name) {
			t.Errorf("runOwnedNamespaceName(%q) = false, want true", name)
		}
	}
}
