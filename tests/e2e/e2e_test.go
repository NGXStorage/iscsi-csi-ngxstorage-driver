package e2e

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ngxDriverName = "iscsi.csi.ngxstorage.com"
	probeTimeout  = 10 * time.Minute
	// namespacePrefix marks every namespace this suite creates as run-owned.
	// Requiring it rejects the shared default namespace, empty names and any
	// foreign/system namespace, so cleanup can never touch objects the run
	// does not own. GenerateName on objects is NOT a substitute for this
	// namespace boundary plus the create-UID delete preconditions below.
	namespacePrefix = "quartet-e2e-"
)

func TestNGXiSCSI(t *testing.T) {
	if os.Getenv("E2E_RUN") != "1" {
		t.Skip("set E2E_RUN=1 to run against a Kubernetes cluster")
	}

	client, err := kubernetesClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// One run-owned namespace per run carries every object this suite creates.
	// Its create UID is captured so cleanup can delete it with an atomic
	// UID precondition; cleanup runs even after the suite context is canceled
	// and any residue fails the test instead of being silently left behind.
	ns, nsUID := createRunNamespace(ctx, t, client)
	defer deleteRunNamespace(context.WithoutCancel(ctx), t, client, ns, nsUID)

	t.Run("dynamic-provisioning", func(t *testing.T) {
		testDynamicProvisioning(ctx, t, client, ns)
	})
	t.Run("controller-expansion", func(t *testing.T) {
		testControllerExpansion(ctx, t, client, ns)
	})
}

func kubernetesClient() (kubernetes.Interface, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.Timeout = 30 * time.Second
	return kubernetes.NewForConfig(config)
}

// createRunNamespace creates the run-owned namespace and returns its name and
// create UID. An explicit E2E_NAMESPACE override is honored only when it looks
// run-owned (namespacePrefix + a valid DNS label); the shared default namespace
// and empty names are rejected. The namespace is always created here so the run
// provably owns it and holds its create UID; a pre-existing name fails closed.
func createRunNamespace(ctx context.Context, t *testing.T, client kubernetes.Interface) (string, types.UID) {
	t.Helper()
	ns := &corev1.Namespace{}
	if override := os.Getenv("E2E_NAMESPACE"); override != "" {
		if !runOwnedNamespaceName(override) {
			t.Fatalf("E2E_NAMESPACE=%q is not run-owned; use a %q-prefixed DNS label (never a shared namespace)", override, namespacePrefix)
		}
		ns.ObjectMeta = metav1.ObjectMeta{Name: override}
	} else {
		ns.ObjectMeta = metav1.ObjectMeta{GenerateName: namespacePrefix}
	}
	created, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create run-owned namespace: %v", err)
	}
	return created.Name, created.UID
}

// runOwnedNamespaceName reports whether name is a run-owned namespace name. The
// prefix requirement rejects empty and shared names (including the default
// namespace) without needing to enumerate them.
func runOwnedNamespaceName(name string) bool {
	if !strings.HasPrefix(name, namespacePrefix) || len(name) <= len(namespacePrefix) {
		return false
	}
	return len(validation.IsDNS1123Label(name)) == 0
}

// deleteRunNamespace removes the run-owned namespace with the create-UID
// precondition carried IN DeleteOptions (the API server enforces it atomically,
// so a replacement namespace is never deleted by name). It then waits for the
// namespace to disappear; any failure fails the test — never silent residue.
func deleteRunNamespace(ctx context.Context, t *testing.T, client kubernetes.Interface, name string, uid types.UID) {
	t.Helper()
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := client.CoreV1().Namespaces().Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete run-owned namespace %s (uid %s): %v", name, uid, err)
		return
	}
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, probeTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		t.Errorf("run-owned namespace %s still present after cleanup: %v", name, err)
	}
}

func testDynamicProvisioning(ctx context.Context, t *testing.T, client kubernetes.Interface, ns string) {
	sc := newStorageClass("ngx-e2e-dynamic-")
	createdSC, err := client.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create StorageClass: %v", err)
	}
	defer deleteStorageClass(context.WithoutCancel(ctx), t, client, createdSC.Name, createdSC.UID)

	pvc := newPVC(createdSC.Name, "ngx-e2e-dynamic-", ns)
	createdPVC, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	defer deletePVC(context.WithoutCancel(ctx), t, client, ns, createdPVC.Name, createdPVC.UID)
	waitPVCBound(ctx, t, client, ns, createdPVC.Name)

	pod := newProbePod(createdPVC.Name, "ngx-e2e-dynamic-", ns)
	createdPod, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create probe pod: %v", err)
	}
	defer deletePod(context.WithoutCancel(ctx), t, client, ns, createdPod.Name, createdPod.UID)
	waitPodRunning(ctx, t, client, ns, createdPod.Name)

	logs, err := client.CoreV1().Pods(ns).GetLogs(createdPod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatalf("read probe pod logs: %v", err)
	}
	if !strings.Contains(string(logs), "ngx-e2e-ok") {
		t.Fatalf("probe marker missing from pod logs: %q", logs)
	}
}

func testControllerExpansion(ctx context.Context, t *testing.T, client kubernetes.Interface, ns string) {
	sc := newStorageClass("ngx-e2e-resize-")
	createdSC, err := client.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create StorageClass: %v", err)
	}
	defer deleteStorageClass(context.WithoutCancel(ctx), t, client, createdSC.Name, createdSC.UID)

	pvc := newPVC(createdSC.Name, "ngx-e2e-resize-", ns)
	createdPVC, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	defer deletePVC(context.WithoutCancel(ctx), t, client, ns, createdPVC.Name, createdPVC.UID)
	waitPVCBound(ctx, t, client, ns, createdPVC.Name)

	pod := newProbePod(createdPVC.Name, "ngx-e2e-resize-", ns)
	createdPod, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create resize probe pod: %v", err)
	}
	defer deletePod(context.WithoutCancel(ctx), t, client, ns, createdPod.Name, createdPod.UID)
	waitPodRunning(ctx, t, client, ns, createdPod.Name)
	beforeKiB := waitPodFilesystemSize(ctx, t, client, ns, createdPod.Name, 0)

	patch := []byte(`{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`)
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Patch(ctx, createdPVC.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("expand PVC: %v", err)
	}
	waitPVCCapacity(ctx, t, client, ns, createdPVC.Name, resource.MustParse("2Gi"))
	afterKiB := waitPodFilesystemSize(ctx, t, client, ns, createdPod.Name, beforeKiB)
	t.Logf("filesystem grew: %d KiB -> %d KiB; data marker preserved", beforeKiB, afterKiB)
}

func newStorageClass(prefix string) *storagev1.StorageClass {
	allowExpansion := true
	return &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{GenerateName: prefix},
		Provisioner:          ngxDriverName,
		AllowVolumeExpansion: &allowExpansion,
		ReclaimPolicy:        func() *corev1.PersistentVolumeReclaimPolicy { p := corev1.PersistentVolumeReclaimDelete; return &p }(),
	}
}

func newPVC(storageClassName, prefix, ns string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
}

func newProbePod(pvcName, prefix, ns string) *corev1.Pod {
	image := os.Getenv("E2E_TEST_IMAGE")
	if image == "" {
		image = "busybox:1.36"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "probe", Image: image, Command: []string{"sh", "-c", `printf ngx-e2e-ok >/data/marker; while true; do if [ "$(cat /data/marker)" != ngx-e2e-ok ]; then echo ngx-e2e-data-error; exit 1; fi; df -Pk /data | awk 'NR==2 {print "ngx-e2e-ok capacity-kib=" $2}'; sleep 2; done`}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
			Volumes:       []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}}}},
		},
	}
}

func waitPodFilesystemSize(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string, largerThanKiB int64) int64 {
	var sizeKiB int64
	waitFor(ctx, t, "filesystem size and preserved data marker", func(ctx context.Context) (bool, error) {
		one := int64(1)
		logs, err := client.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{TailLines: &one}).DoRaw(ctx)
		if err != nil {
			return false, err
		}
		if strings.Contains(string(logs), "ngx-e2e-data-error") {
			return false, fmt.Errorf("probe data marker changed")
		}
		if !strings.Contains(string(logs), "ngx-e2e-ok") {
			return false, nil
		}
		for _, field := range strings.Fields(string(logs)) {
			if strings.HasPrefix(field, "capacity-kib=") {
				sizeKiB, err = strconv.ParseInt(strings.TrimPrefix(field, "capacity-kib="), 10, 64)
				return sizeKiB > largerThanKiB, err
			}
		}
		return false, nil
	})
	return sizeKiB
}

func waitPVCBound(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string) {
	waitFor(ctx, t, "PVC to bind", func(ctx context.Context) (bool, error) {
		pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		return pvc != nil && pvc.Status.Phase == corev1.ClaimBound, err
	})
}

func waitPVCCapacity(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string, expected resource.Quantity) {
	previous := ""
	waitFor(ctx, t, "PVC expansion", func(ctx context.Context) (bool, error) {
		pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		capacity := pvc.Status.Capacity[corev1.ResourceStorage]
		observed := fmt.Sprintf("capacity=%s conditions=%v resize=%v", capacity.String(), pvc.Status.Conditions, pvc.Status.AllocatedResourceStatuses)
		if observed != previous {
			t.Logf("PVC %s expansion: expected=%s %s", name, expected.String(), observed)
			previous = observed
		}
		return capacity.Cmp(expected) == 0, nil
	})
}

func waitPodRunning(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string) {
	waitFor(ctx, t, "probe pod to run", func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("probe pod failed: %s", pod.Status.Reason)
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	})
}

func waitFor(ctx context.Context, t *testing.T, description string, condition wait.ConditionWithContextFunc) {
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, probeTimeout, true, condition); err != nil {
		t.Fatalf("wait for %s: %v", description, err)
	}
}

// Every delete below carries the object's create UID in DeleteOptions
// preconditions so the API server atomically refuses to delete a replacement
// object created under the same name. GenerateName alone is not an ownership
// boundary; the UID precondition plus the run-owned namespace are.

func deleteStorageClass(ctx context.Context, t *testing.T, client kubernetes.Interface, name string, uid types.UID) {
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := client.StorageV1().StorageClasses().Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete StorageClass %s (uid %s): %v", name, uid, err)
	}
}

func deletePVC(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string, uid types.UID) {
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete PVC %s/%s (uid %s): %v", ns, name, uid, err)
	}
}

func deletePod(ctx context.Context, t *testing.T, client kubernetes.Interface, ns, name string, uid types.UID) {
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := client.CoreV1().Pods(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete pod %s/%s (uid %s): %v", ns, name, uid, err)
	}
}
