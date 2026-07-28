package vfhygiene

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	typedv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sriovnetworkv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/vars"
)

// Event reasons emitted by the VF hygiene feature.
const (
	// ReasonVFReleaseDetected is emitted when the kernel signals that a VF
	// has been returned to the host and the VF is confirmed to have no live
	// consumer. This is the release notification Kubernetes itself does not
	// provide: the kubelet device-plugin API has an Allocate RPC and no Free
	// RPC, so nothing in the cluster otherwise reports that a VF came back.
	ReasonVFReleaseDetected = "VFReleaseDetected"
	// ReasonVFStateRestored is emitted after a released VF's state has been
	// restored to its trusted baseline.
	ReasonVFStateRestored = "VFStateRestored"
	// ReasonVFRestoreFailed is emitted when restoring a released VF failed.
	// A VF handed to a new workload while still carrying the previous
	// consumer's state can disrupt the datapath, so this is a Warning rather
	// than an informational message.
	ReasonVFRestoreFailed = "VFRestoreFailed"
)

// KubeRecorder publishes VF hygiene outcomes as Kubernetes Events on this
// node's SriovNetworkNodeState.
//
// This is what makes the feature observable from the cluster rather than only
// from node logs. Restoring state on a device the operator never configured is
// a behaviour change, so an operator needs to be able to see it happen with
// kubectl, and to alert on the failure case.
//
// Events are attached to the SriovNetworkNodeState because that is the object
// that already represents per-node SR-IOV state, so `kubectl describe
// sriovnetworknodestate <node>` shows the VF lifecycle alongside everything
// else about that node's SR-IOV configuration.
type KubeRecorder struct {
	client           client.Client
	recorder         record.EventRecorder
	eventBroadcaster record.EventBroadcaster
}

// NewKubeRecorder creates a recorder that emits events from the config-daemon.
func NewKubeRecorder(c client.Client, kubeclient kubernetes.Interface, s *runtime.Scheme) *KubeRecorder {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(4)
	broadcaster.StartRecordingToSink(&typedv1core.EventSinkImpl{Interface: kubeclient.CoreV1().Events("")})

	return &KubeRecorder{
		client:           c,
		recorder:         broadcaster.NewRecorder(s, corev1.EventSource{Component: "config-daemon"}),
		eventBroadcaster: broadcaster,
	}
}

// Shutdown closes the event broadcaster.
func (r *KubeRecorder) Shutdown() {
	r.eventBroadcaster.Shutdown()
}

// VFReleased reports that the kernel signalled a VF returning to the host and
// that the VF has no live consumer.
func (r *KubeRecorder) VFReleased(vf VF) {
	r.emit(corev1.EventTypeNormal, ReasonVFReleaseDetected,
		fmt.Sprintf("VF %s (%s) on PF %s was released by its consumer",
			vf.PCIAddress, netdevOrNone(vf), vf.PFPCIAddress))
}

// VFRestored reports a successful restore.
func (r *KubeRecorder) VFRestored(vf VF, attrs []Attribute) {
	r.emit(corev1.EventTypeNormal, ReasonVFStateRestored,
		fmt.Sprintf("Restored released VF %s (%s) on PF %s to its trusted baseline: %s",
			vf.PCIAddress, netdevOrNone(vf), vf.PFPCIAddress, joinAttrs(attrs)))
}

// VFRestoreFailed reports a failed restore as a Warning.
func (r *KubeRecorder) VFRestoreFailed(vf VF, attrs []Attribute, err error) {
	r.emit(corev1.EventTypeWarning, ReasonVFRestoreFailed,
		fmt.Sprintf("Failed to restore released VF %s (%s) on PF %s (%s): %v. "+
			"The VF may be returned to the pool with state left by its previous consumer",
			vf.PCIAddress, netdevOrNone(vf), vf.PFPCIAddress, joinAttrs(attrs), err))
}

// emit attaches an event to this node's SriovNetworkNodeState.
//
// Event spam is handled by the client-go event correlator, which aggregates
// repeats of the same reason on the same object -- relevant here because a
// flapping workload can release the same VF repeatedly.
func (r *KubeRecorder) emit(eventType, reason, msg string) {
	nodeState := &sriovnetworkv1.SriovNetworkNodeState{}
	err := r.client.Get(context.Background(),
		client.ObjectKey{Namespace: vars.Namespace, Name: vars.NodeName}, nodeState)
	if err != nil {
		log.Log.V(2).Error(err, "failed to fetch node state, skipping event",
			"node", vars.NodeName, "reason", reason)
		return
	}
	r.recorder.Event(nodeState, eventType, reason, msg)
}

func netdevOrNone(vf VF) string {
	if vf.NetdevName == "" {
		return "no netdev, userspace driver"
	}
	return vf.NetdevName
}

func joinAttrs(attrs []Attribute) string {
	parts := make([]string, 0, len(attrs))
	for _, a := range attrs {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, ", ")
}
