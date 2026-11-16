package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

func TestNodeDisks(t *testing.T) {
	sixteen := int32(16)
	csi := &storagev1.CSINode{Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{
		{Name: "disk.csi.azure.com", Allocatable: &storagev1.VolumeNodeResources{Count: &sixteen}},
		{Name: "file.csi.azure.com"},
	}}}
	n := &corev1.Node{Status: corev1.NodeStatus{VolumesAttached: []corev1.AttachedVolume{
		{Name: "kubernetes.io/csi/disk.csi.azure.com^/subscriptions/x/resourceGroups/rg/providers/Microsoft.Compute/disks/pvc-1"},
		{Name: "kubernetes.io/csi/disk.csi.azure.com^/subscriptions/x/resourceGroups/rg/providers/Microsoft.Compute/disks/pvc-2"},
	}}}
	d := nodeDisks(n, csi)
	if d.Attached != 2 || d.Limit != 16 || d.cell() != "2/16" || len(d.Drivers) != 1 || d.Drivers[0].Volumes[0] != "pvc-1" {
		t.Fatalf("with limit: %+v", d)
	}
	if d := nodeDisks(n, nil); d.cell() != "2" || d.Limit != -1 {
		t.Fatalf("without CSINode: %+v", d)
	}
	if d := nodeDisks(&corev1.Node{}, nil); d.cell() != "0" {
		t.Fatalf("no disks: %+v", d)
	}
}
