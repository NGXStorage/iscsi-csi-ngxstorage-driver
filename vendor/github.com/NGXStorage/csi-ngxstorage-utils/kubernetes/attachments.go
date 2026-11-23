package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type AttachmentLookup interface {
	RemainingNodes(context.Context, string, string, string) ([]string, error)
}

// NodeLookup reports whether a Kubernetes node exists. CSI publish/unpublish
// must fail with NotFound for a node the cluster does not have.
type NodeLookup interface {
	NodeExists(context.Context, string) (bool, error)
}

type attachmentAPI interface {
	PersistentVolumes(context.Context) ([]corev1.PersistentVolume, error)
	VolumeAttachments(context.Context) ([]storagev1.VolumeAttachment, error)
	Node(context.Context, string) (*corev1.Node, error)
}

type clientsetAttachmentAPI struct{ client clientset.Interface }

func (a clientsetAttachmentAPI) PersistentVolumes(ctx context.Context) ([]corev1.PersistentVolume, error) {
	list, err := a.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (a clientsetAttachmentAPI) VolumeAttachments(ctx context.Context) ([]storagev1.VolumeAttachment, error) {
	list, err := a.client.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (a clientsetAttachmentAPI) Node(ctx context.Context, name string) (*corev1.Node, error) {
	return a.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

type Lookup struct{ api attachmentAPI }

func NewAttachmentLookup() (*Lookup, error) {
	return newLookupFromEnvironment(0, 0)
}

// NewNodeLookup creates the same lookup adapter with bounded API pacing for
// the NFS controller's Node-only API checks.
func NewNodeLookup() (*Lookup, error) {
	return newLookupFromEnvironment(30, 60)
}

func newLookupFromEnvironment(qps float32, burst int) (*Lookup, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	if qps > 0 {
		config.QPS = qps
		config.Burst = burst
	}
	client, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Lookup{api: clientsetAttachmentAPI{client: client}}, nil
}

func (l *Lookup) RemainingNodes(ctx context.Context, driverName, volumeHandle, excludingNodeID string) ([]string, error) {
	persistentVolumes, err := l.api.PersistentVolumes(ctx)
	if err != nil {
		return nil, err
	}
	nodes := map[string]struct{}{}
	var pvCount int
	var volumeAttachments []storagev1.VolumeAttachment
	for _, pv := range persistentVolumes {
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driverName || pv.Spec.CSI.VolumeHandle != volumeHandle {
			continue
		}
		pvCount++
		if pvCount > 1 {
			return nil, ErrAmbiguousVolumePV{Count: pvCount}
		}
		if volumeAttachments == nil {
			listed, listErr := l.api.VolumeAttachments(ctx)
			if listErr != nil {
				return nil, listErr
			}
			volumeAttachments = listed
		}
		for _, va := range volumeAttachments {
			if va.Spec.Source.PersistentVolumeName != nil && *va.Spec.Source.PersistentVolumeName == pv.Name && va.Spec.NodeName != excludingNodeID {
				nodes[va.Spec.NodeName] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(nodes))
	for node := range nodes {
		out = append(out, node)
	}
	sort.Strings(out)
	return out, nil
}

// NodeExists reports whether a Kubernetes node with the given name exists.
// API failures are propagated so callers fail closed instead of treating an
// unreadable cluster as "node absent".
func (l *Lookup) NodeExists(ctx context.Context, name string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, nil
	}
	if _, err := l.api.Node(ctx, name); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get node %q: %w", name, err)
	}
	return true, nil
}

type ErrAmbiguousVolumePV struct{ Count int }

func (e ErrAmbiguousVolumePV) Error() string {
	return "multiple persistent volumes match the CSI driver and volume handle"
}
