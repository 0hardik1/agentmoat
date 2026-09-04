// Package classifier: the gpu-passthrough refinement.
//
// The static rule can only say "this workload asks for a GPU". Whether
// gVisor can serve that request depends on the cluster: which card the GPU
// nodes have, whether they are MIG-sliced, and whether the installed runsc
// lists the host driver (pkg/preflight/gpu.go collects those facts;
// pkg/probe fills in the driver list). This file turns the facts into a
// verdict:
//
//	MIG slice requested (nvidia.com/mig-*)                   -> error
//	no GPU facts                                            -> unchanged
//	every eligible GPU node: supported card + listed driver -> info
//	some eligible nodes usable, others not                  -> warn, pin advice
//	supported card, driver not probed yet                   -> unchanged, note
//	supported card, driver not listed by runsc              -> error
//	GPU nodes without GFD labels                            -> unchanged, note
//	only unsupported cards / MIG nodes                      -> error
//
// "Eligible" means matching the RuntimeClass nodeSelector when at least one
// GPU node does; otherwise every GPU node is considered, since without a
// selector nothing says which nodes are the gVisor ones.
package classifier

import (
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

// refineGPUPassthrough is the Rule.Refine hook for gpu-passthrough.
func refineGPUPassthrough(w scanner.Workload, facts *schema.ClusterFacts, base Severity) (Refinement, bool) {
	if res := migResourceRequested(w); res != "" {
		return Refinement{
			Severity: SeverityError,
			Note:     fmt.Sprintf("The request is for a MIG slice (%s); gVisor nvproxy does not support MIG.", res),
		}, true
	}
	if facts == nil || facts.GPU == nil || facts.GPU.Nodes == 0 {
		return Refinement{}, false
	}
	return refineFromGPUFacts(facts.GPU, base), true
}

// migResourceRequested returns the first (sorted) nvidia.com/mig-* resource
// any container requests, or "".
func migResourceRequested(w scanner.Workload) string {
	var found []string
	for _, c := range allContainers(w.PodSpec) {
		for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
			for name := range list {
				if strings.HasPrefix(string(name), schema.NvidiaMIGResourcePrefix) {
					found = append(found, string(name))
				}
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}

// gpuTally buckets the eligible GPU node groups by what stands between
// them and a working nvproxy.
type gpuTally struct {
	scope                                           string
	ready, unconfirmed, driverBad, unknown, blocked []string
}

func tallyGPU(gf *schema.GPUFacts) gpuTally {
	eligible := gf.MatchingNodes > 0
	t := gpuTally{scope: "GPU node(s)"}
	if eligible {
		t.scope = "GPU node(s) matching the RuntimeClass"
	}
	for _, g := range gf.Groups {
		if eligible && g.MatchingNodes == 0 {
			continue
		}
		d := preflight.DescribeGPUGroup(g)
		switch {
		case g.MIG || g.ProductSupport == schema.SupportUnsupported:
			t.blocked = append(t.blocked, d)
		case g.ProductSupport == schema.SupportUnknown:
			t.unknown = append(t.unknown, d)
		case g.DriverSupport == schema.SupportSupported:
			t.ready = append(t.ready, d)
		case g.DriverSupport == schema.SupportUnsupported:
			t.driverBad = append(t.driverBad, d)
		default:
			t.unconfirmed = append(t.unconfirmed, d)
		}
	}
	return t
}

// refineFromGPUFacts applies the decision table in the file header.
func refineFromGPUFacts(gf *schema.GPUFacts, base Severity) Refinement {
	t := tallyGPU(gf)
	runsc := "runsc"
	if gf.Nvproxy != nil && gf.Nvproxy.RunscVersion != "" {
		runsc = "runsc " + gf.Nvproxy.RunscVersion
	}
	var others []string
	others = append(others, t.unconfirmed...)
	others = append(others, t.driverBad...)
	others = append(others, t.unknown...)
	others = append(others, t.blocked...)

	switch {
	case len(t.ready) > 0 && len(others) == 0:
		return Refinement{Severity: SeverityInfo, Note: fmt.Sprintf(
			"Cluster facts: %s nvproxy supports the card and host driver on every %s (%s).",
			runsc, t.scope, strings.Join(t.ready, "; "))}
	case len(t.ready) > 0:
		return Refinement{Severity: SeverityWarn, Note: fmt.Sprintf(
			"Cluster facts: %s nvproxy supports %s, but other %s could not be confirmed or are unsupported (%s); pin the workload with a nodeSelector on %s.",
			runsc, strings.Join(t.ready, "; "), t.scope, strings.Join(others, "; "), schema.GFDProductLabel)}
	case len(t.unconfirmed) > 0:
		return Refinement{Severity: base, Note: fmt.Sprintf(
			"Cluster facts: the %s carry an nvproxy-supported card (%s) but the host driver has not been checked against runsc; run 'agentmoat probe nvproxy --dry-run=false'.",
			t.scope, strings.Join(t.unconfirmed, "; "))}
	case len(t.driverBad) > 0:
		return Refinement{Severity: SeverityError, Note: fmt.Sprintf(
			"Cluster facts: %s nvproxy does not list the host driver on the %s with a supported card (%s).",
			runsc, t.scope, strings.Join(t.driverBad, "; "))}
	case len(t.unknown) > 0:
		return Refinement{Severity: base, Note: fmt.Sprintf(
			"Cluster facts: the %s carry no GPU Feature Discovery labels (%s), so the card cannot be checked.",
			t.scope, strings.Join(t.unknown, "; "))}
	default:
		return Refinement{Severity: SeverityError, Note: fmt.Sprintf(
			"Cluster facts: no %s has a card gVisor nvproxy supports (%s).",
			t.scope, strings.Join(t.blocked, "; "))}
	}
}
