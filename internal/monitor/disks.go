package monitor

import (
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Disks attached to a node, per storage driver, against the attach limit the driver advertises on
// the node's CSINode (e.g. Azure Disk's per-VM-size data-disk count). Limit is -1 when the driver
// doesn't publish one.

type DriverDisks struct {
	Driver   string   `json:"driver"`
	Attached int      `json:"attached"`
	Limit    int      `json:"limit"`
	Volumes  []string `json:"volumes,omitempty"`
}

type NodeDisks struct {
	Attached int           `json:"attached"`
	Limit    int           `json:"limit"` // sum of advertised limits; -1 when none advertised
	Drivers  []DriverDisks `json:"drivers"`
}

// attachedDriver splits "kubernetes.io/csi/<driver>^<volume handle>" (CSI) or the in-tree
// "kubernetes.io/<plugin>/<volume>" forms into driver and volume.
func attachedDriver(name string) (driver, volume string) {
	if rest, ok := strings.CutPrefix(name, "kubernetes.io/csi/"); ok {
		d, v, _ := strings.Cut(rest, "^")
		return d, v
	}
	if rest, ok := strings.CutPrefix(name, "kubernetes.io/"); ok {
		d, v, _ := strings.Cut(rest, "/")
		return "kubernetes.io/" + d, v
	}
	return "other", name
}

func nodeDisks(n *corev1.Node, csi *storagev1.CSINode) NodeDisks {
	by := map[string]*DriverDisks{}
	get := func(d string) *DriverDisks {
		if by[d] == nil {
			by[d] = &DriverDisks{Driver: d, Limit: -1}
		}
		return by[d]
	}
	if csi != nil {
		for _, d := range csi.Spec.Drivers {
			if d.Allocatable != nil && d.Allocatable.Count != nil {
				get(d.Name).Limit = int(*d.Allocatable.Count)
			}
		}
	}
	for _, v := range n.Status.VolumesAttached {
		d, vol := attachedDriver(string(v.Name))
		dd := get(d)
		dd.Attached++
		// The handle is usually a cloud path; its last segment is the disk (often the PV) name.
		if i := strings.LastIndex(vol, "/"); i >= 0 {
			vol = vol[i+1:]
		}
		dd.Volumes = append(dd.Volumes, vol)
	}
	out := NodeDisks{Limit: -1, Drivers: []DriverDisks{}}
	for _, d := range by {
		if d.Attached == 0 && d.Limit < 0 {
			continue
		}
		slices.Sort(d.Volumes)
		out.Attached += d.Attached
		if d.Limit >= 0 {
			out.Limit = max(out.Limit, 0) + d.Limit
		}
		out.Drivers = append(out.Drivers, *d)
	}
	slices.SortFunc(out.Drivers, func(a, b DriverDisks) int { return strings.Compare(a.Driver, b.Driver) })
	return out
}

// diskCell is the list column: "3/16" when a limit is advertised, else "3".
func (d NodeDisks) cell() string {
	if d.Limit >= 0 {
		return strconv.Itoa(d.Attached) + "/" + strconv.Itoa(d.Limit)
	}
	return strconv.Itoa(d.Attached)
}

func (c *cluster) csiNode(name string) *storagev1.CSINode {
	if c.csinodes == nil || !c.synced("csinodes") {
		return nil
	}
	n, err := c.csinodes.Get(name)
	if err != nil {
		return nil
	}
	return n
}
