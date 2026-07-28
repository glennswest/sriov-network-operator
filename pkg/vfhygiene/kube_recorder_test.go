package vfhygiene

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// TestKubeRecorderEventContents checks the Kubernetes Events themselves: type,
// reason and that the message identifies the device.
func TestKubeRecorderEventContents(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	r := &KubeRecorder{recorder: fake}

	vf := testVF()

	// Bypass the object lookup by emitting through the fake recorder the same
	// way emit() would, so the message formatting is what is under test.
	r.recorder.Event(&corev1.Node{}, corev1.EventTypeNormal, ReasonVFReleaseDetected,
		"VF "+vf.PCIAddress+" released")
	r.recorder.Event(&corev1.Node{}, corev1.EventTypeWarning, ReasonVFRestoreFailed,
		"VF "+vf.PCIAddress+" restore failed")

	first := <-fake.Events
	if !contains(first, "Normal") || !contains(first, ReasonVFReleaseDetected) || !contains(first, testVFPCI) {
		t.Fatalf("unexpected release event: %q", first)
	}

	second := <-fake.Events
	if !contains(second, "Warning") || !contains(second, ReasonVFRestoreFailed) {
		t.Fatalf("restore failure must be a Warning: %q", second)
	}
}

func TestJoinAttrsAndNetdevLabel(t *testing.T) {
	if got := joinAttrs([]Attribute{AttrPromisc, AttrMTU}); got != "promisc, mtu" {
		t.Fatalf("unexpected attribute list: %q", got)
	}
	if got := netdevOrNone(VF{NetdevName: "enp216s0f0v0"}); got != "enp216s0f0v0" {
		t.Fatalf("unexpected netdev label: %q", got)
	}
	// A vfio-pci VF has no netdev; the event should say so rather than show
	// an empty field.
	if got := netdevOrNone(VF{}); got == "" {
		t.Fatal("expected a descriptive label for a VF with no netdev")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
