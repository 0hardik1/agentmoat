// Package preflight: GPU facts and nvproxy support.
//
// gVisor runs CUDA workloads through nvproxy, a Sentry subsystem that
// forwards the NVIDIA driver ioctl surface to the host driver. Two things
// decide whether that works for a given cluster, and neither is visible
// from the workload spec:
//
//   - the card model: nvproxy supports a short list (NvproxySupportedProducts),
//     published in the gVisor GPU guide;
//   - the host driver version: each runsc release ships an explicit list of
//     driver versions it can proxy (`runsc nvproxy list-supported-drivers`),
//     and a version not on the list fails at the first GPU ioctl.
//
// GPU Feature Discovery (GFD) publishes the card model and the driver
// version as node labels, so the first check is a node-list read. The
// second needs the runsc binary itself, which is what pkg/probe fetches;
// until it has run, the driver verdict is "unknown".
package preflight

import (
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NvproxySupportedProducts lists the NVIDIA card families gVisor nvproxy
// supports, from https://gvisor.dev/docs/user_guide/gpu/. A GFD product
// label is matched token-wise, splitting on dashes, underscores, and
// spaces: "Tesla-T4" matches T4, "NVIDIA-A100-SXM4-40GB" matches A100,
// "NVIDIA-A10" does not match A10G, "NVIDIA-L40S" does not match L4.
// Revisit this list when bumping the gVisor pin (docs/gvisor-version.md).
var NvproxySupportedProducts = []string{"T4", "A100", "A10G", "L4", "H100"}

// ProductSupport decides whether nvproxy supports the card a GFD
// gpu.product label names. Empty (no GFD) is unknown.
func ProductSupport(product string) schema.SupportStatus {
	if product == "" {
		return schema.SupportUnknown
	}
	tokens := strings.FieldsFunc(strings.ToUpper(product), func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for _, tok := range tokens {
		for _, want := range NvproxySupportedProducts {
			if tok == want {
				return schema.SupportSupported
			}
		}
	}
	return schema.SupportUnsupported
}

// DriverSupport decides whether the probed runsc lists the host driver.
// Unknown until the probe has run (nv nil) or when GFD published no
// driver label.
func DriverSupport(driver string, nv *schema.NvproxyFacts) schema.SupportStatus {
	if driver == "" || nv == nil {
		return schema.SupportUnknown
	}
	for _, d := range nv.SupportedDrivers {
		if d == driver {
			return schema.SupportSupported
		}
	}
	return schema.SupportUnsupported
}

// RefreshGPUSupport recomputes the support verdicts on every GPU node
// group. Collect calls it once; the probe calls it again after filling
// GPU.Nvproxy, which is what turns the driver verdicts from unknown into
// supported or unsupported.
func RefreshGPUSupport(facts *schema.ClusterFacts) {
	if facts == nil || facts.GPU == nil {
		return
	}
	for i := range facts.GPU.Groups {
		g := &facts.GPU.Groups[i]
		g.ProductSupport = ProductSupport(g.Product)
		g.DriverSupport = DriverSupport(g.Driver, facts.GPU.Nvproxy)
	}
}

// HasGPU reports whether the node advertises an NVIDIA GPU: an
// nvidia.com/* entry in status.capacity (the device plugin) or a GFD
// product label (GFD can run without the device plugin, and vice versa).
func HasGPU(n *corev1.Node) bool {
	if n.Labels[schema.GFDProductLabel] != "" {
		return true
	}
	for name := range n.Status.Capacity {
		if strings.HasPrefix(string(name), schema.NvidiaResourcePrefix) {
			return true
		}
	}
	return false
}

// gpuKey is the grouping key: the triple that decides nvproxy support.
type gpuKey struct {
	product string
	driver  string
	mig     bool
}

// gpuKeyOf reads the GFD labels of a GPU node. The driver comes from the
// full-version label when present, else from the legacy major/minor/rev
// triple; MIG is anything other than strategy "none".
func gpuKeyOf(n *corev1.Node) gpuKey {
	k := gpuKey{product: n.Labels[schema.GFDProductLabel]}
	k.driver = n.Labels[schema.GFDDriverVersionLabel]
	if k.driver == "" {
		major, minor := n.Labels[schema.GFDDriverMajorLabel], n.Labels[schema.GFDDriverMinorLabel]
		if major != "" && minor != "" {
			k.driver = major + "." + minor
			if rev := n.Labels[schema.GFDDriverRevLabel]; rev != "" {
				k.driver += "." + rev
			}
		}
	}
	if strategy := n.Labels[schema.GFDMIGStrategyLabel]; strategy != "" && strategy != "none" {
		k.mig = true
	}
	return k
}

// collectGPU groups the GPU nodes by (product, driver, MIG). Returns nil
// when no node has a GPU, so ClusterFacts.GPU stays absent on the common
// CPU-only cluster.
func collectGPU(nodes []corev1.Node, sel labels.Selector) *schema.GPUFacts {
	gf := &schema.GPUFacts{}
	groups := map[gpuKey]*schema.GPUNodeGroup{}
	for i := range nodes {
		n := &nodes[i]
		if !HasGPU(n) {
			continue
		}
		k := gpuKeyOf(n)
		matches := sel != nil && sel.Matches(labels.Set(n.Labels))
		gf.Nodes++
		if matches {
			gf.MatchingNodes++
		}
		if k.mig {
			gf.MIGNodes++
		}
		grp := groups[k]
		if grp == nil {
			grp = &schema.GPUNodeGroup{Product: k.product, Driver: k.driver, MIG: k.mig}
			groups[k] = grp
		}
		grp.Nodes++
		if matches {
			grp.MatchingNodes++
		}
	}
	if gf.Nodes == 0 {
		return nil
	}
	for _, grp := range groups {
		gf.Groups = append(gf.Groups, *grp)
	}
	sort.Slice(gf.Groups, func(i, j int) bool {
		a, b := gf.Groups[i], gf.Groups[j]
		if a.Product != b.Product {
			return a.Product < b.Product
		}
		if a.Driver != b.Driver {
			return a.Driver < b.Driver
		}
		return !a.MIG && b.MIG
	})
	return gf
}

// DescribeGPUGroup renders one group for messages: the card, the driver,
// and the node counts. Shared by the findings and the classifier note so
// the operator reads the same words in both places.
func DescribeGPUGroup(g schema.GPUNodeGroup) string {
	product := g.Product
	if product == "" {
		product = "unlabeled card"
	}
	driver := g.Driver
	if driver == "" {
		driver = "unknown"
	}
	var b strings.Builder
	b.WriteString(product)
	if g.MIG {
		b.WriteString(" (MIG)")
	}
	b.WriteString(" driver ")
	b.WriteString(driver)
	b.WriteString(" on ")
	b.WriteString(itoa(g.Nodes))
	b.WriteString(" node(s)")
	if g.MatchingNodes > 0 {
		b.WriteString(", ")
		b.WriteString(itoa(g.MatchingNodes))
		b.WriteString(" matching the RuntimeClass")
	}
	return b.String()
}

// itoa avoids importing strconv into a file that otherwise only formats
// text; kept tiny on purpose.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
