// Package preflight: sandbox co-location.
//
// A sandbox protects the node from the pod. It does not protect a pod from
// the node: kubelet, the CRI, and the EKS Pod Identity agent all run on the
// node and hold or can fetch every pod's credentials. So the node is part
// of every sandboxed pod's trusted computing base, and anything that turns
// into node root reaches those credentials. The cheapest such thing is
// usually a container escape from a plain runc pod on the same node, which
// is exactly the class of bug the sandbox was meant to take off the table.
//
// The check answers one question: which nodes run sandboxed pods next to
// runc workloads? It covers two kinds of sandbox:
//
//   - the target RuntimeClass (gVisor in agentmoat's case). Its
//     scheduling.nodeSelector decides which nodes are gVisor nodes, but
//     nothing stops a runc pod from landing there unless the nodes are
//     tainted.
//   - Amazon EKS Nitro Sandboxes (RuntimeClasses isolated-sandbox and
//     confidential-sandbox). The EKS user guide's Nitro Sandbox security
//     model says the parent instance "can mint Pod Identity credentials
//     for any pod scheduled on the node", and the EKS AL2023 pod-isolation
//     AMI keeps runc as the default runtime and only labels its nodes
//     eks.amazonaws.com/nitro-sandbox=true; it adds no taint. So runc pods
//     share these nodes unless the operator says otherwise.
//
// What it deliberately does not do: judge how broad a pod's IAM role is.
// That needs the AWS API (EKS ListPodIdentityAssociations, IAM), and
// agentmoat talks only to Kubernetes. It reports which sandboxed pods carry
// AWS credentials so an operator knows which roles to review.
//
// Shape mirrors the rest of the package: collectSandbox is the only part
// that reads the API (pods on the candidate nodes, get/list only);
// BuildSandboxFacts and sandboxFindings are pure.
package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// Environment variables the EKS mutating webhooks inject into a pod's
// containers. Their presence is how a pod spec tells us the pod gets AWS
// credentials, without reading IAM.
const (
	// EKS Pod Identity: the agent's credential endpoint and the projected
	// token the SDK presents to it.
	envPodIdentityURI   = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
	envPodIdentityToken = "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE" //nolint:gosec // G101: an env var name, not a credential

	// IAM roles for service accounts (IRSA): the role and the projected
	// web identity token.
	envIRSARoleARN   = "AWS_ROLE_ARN"
	envIRSATokenFile = "AWS_WEB_IDENTITY_TOKEN_FILE" //nolint:gosec // G101: an env var name, not a credential
)

// maxNamesInMessage caps how many node or pod names a finding message
// quotes. The full lists are in the facts.
const maxNamesInMessage = 5

// sandboxRuntimeClasses returns the RuntimeClass names that count as
// sandboxed: the target plus the two EKS Nitro Sandbox classes.
func sandboxRuntimeClasses(target string) map[string]bool {
	if target == "" {
		target = schema.DefaultRuntimeClassName
	}
	return map[string]bool{
		target:                                  true,
		schema.NitroIsolatedSandboxRuntimeClass: true,
		schema.NitroConfidentialSandboxRuntimeClass: true,
	}
}

// IsNitroSandboxNode reports whether the node carries the label the EKS
// pod-isolation AMI (and EKS admission) use for Nitro Sandbox capacity.
func IsNitroSandboxNode(n *corev1.Node) bool {
	return n.Labels[schema.NitroSandboxNodeLabel] == schema.NitroSandboxNodeLabelValue
}

// collectSandbox lists the pods on every node that can host sandboxed
// pods and fills facts.Sandbox. Candidates are the nodes matching the
// target RuntimeClass nodeSelector plus the Nitro Sandbox nodes; pods are
// listed per node with a spec.nodeName field selector so a large cluster
// is not listed in full.
//
// Errors never fail the preflight: this is a hardening check, and the
// apply gate must not start refusing on clusters whose RBAC predates it.
// A failed list records Checked=false and the reason.
func collectSandbox(ctx context.Context, client kubernetes.Interface, facts *schema.ClusterFacts, nodes []corev1.Node) {
	candidates := map[string]bool{}
	for _, name := range facts.Nodes.MatchingNames {
		candidates[name] = true
	}
	for i := range nodes {
		if IsNitroSandboxNode(&nodes[i]) {
			candidates[nodes[i].Name] = true
		}
	}
	if len(candidates) == 0 {
		return
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)

	var pods []corev1.Pod
	for _, name := range names {
		list, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", name).String(),
		})
		if err != nil {
			facts.Sandbox = &schema.SandboxFacts{
				Checked: false,
				Reason:  fmt.Sprintf("listing pods on node %s: %v", name, err),
			}
			return
		}
		// Keep only pods actually bound to this node. The API server
		// already filtered, but a client that ignores field selectors
		// (client-go's fake clientset does) would otherwise count every
		// pod once per candidate node.
		for j := range list.Items {
			if list.Items[j].Spec.NodeName == name {
				pods = append(pods, list.Items[j])
			}
		}
	}
	facts.Sandbox = BuildSandboxFacts(facts.RuntimeClass.Name, pods)
}

// BuildSandboxFacts groups pods by node and returns the co-location facts
// for every node that runs at least one sandboxed pod. Nil when none does.
// Pure: same pods in, same facts out (all lists sorted).
func BuildSandboxFacts(targetRuntimeClass string, pods []corev1.Pod) *schema.SandboxFacts {
	sandboxed := sandboxRuntimeClasses(targetRuntimeClass)

	type acc struct {
		classes map[string]bool
		node    schema.SandboxNode
	}
	byNode := map[string]*acc{}
	get := func(name string) *acc {
		a, ok := byNode[name]
		if !ok {
			a = &acc{classes: map[string]bool{}, node: schema.SandboxNode{Name: name}}
			byNode[name] = a
		}
		return a
	}

	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || podFinished(p) {
			continue
		}
		a := get(p.Spec.NodeName)
		rc := ""
		if p.Spec.RuntimeClassName != nil {
			rc = *p.Spec.RuntimeClassName
		}
		ref := p.Namespace + "/" + p.Name
		switch {
		case sandboxed[rc]:
			a.classes[rc] = true
			a.node.SandboxedPods++
			if podHasAWSCredentials(p) {
				a.node.SandboxedPodsWithAWSCredentials = append(a.node.SandboxedPodsWithAWSCredentials, ref)
			}
		case ownedByDaemonSet(p):
			a.node.RuncDaemonSetPods++
		default:
			// Any other RuntimeClass (or none) is treated as runc. A
			// cluster with a third sandbox runtime would pass it as the
			// target RuntimeClass.
			a.node.RuncWorkloadPods = append(a.node.RuncWorkloadPods, ref)
		}
	}

	out := &schema.SandboxFacts{Checked: true}
	for _, a := range byNode {
		if a.node.SandboxedPods == 0 {
			continue
		}
		for rc := range a.classes {
			a.node.RuntimeClasses = append(a.node.RuntimeClasses, rc)
		}
		sort.Strings(a.node.RuntimeClasses)
		sort.Strings(a.node.SandboxedPodsWithAWSCredentials)
		sort.Strings(a.node.RuncWorkloadPods)
		out.Nodes = append(out.Nodes, a.node)
	}
	if len(out.Nodes) == 0 {
		return nil
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	return out
}

// podFinished: Succeeded and Failed pods hold no running containers, so
// they neither escape nor have credentials worth stealing.
func podFinished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// ownedByDaemonSet reports whether a DaemonSet controls the pod.
func ownedByDaemonSet(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// podHasAWSCredentials reports whether any container (init containers
// included) carries the environment the EKS Pod Identity or IRSA webhook
// injects. Either variable of a pair is enough: the webhooks set both, and
// a hand-written spec that sets one still means the pod expects AWS
// credentials from the node.
func podHasAWSCredentials(p *corev1.Pod) bool {
	check := func(cs []corev1.Container) bool {
		for i := range cs {
			for _, e := range cs[i].Env {
				switch e.Name {
				case envPodIdentityURI, envPodIdentityToken, envIRSARoleARN, envIRSATokenFile:
					return true
				}
			}
		}
		return false
	}
	return check(p.Spec.InitContainers) || check(p.Spec.Containers)
}

// sandboxFindings turns facts.Sandbox into findings. None of them blocks:
// co-location is a hardening gap, not a reason the migration cannot land.
func sandboxFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	sb := f.Sandbox
	if sb == nil {
		return nil
	}
	if !sb.Checked {
		return []schema.PreflightFinding{{
			ID:       FindingSandboxColocationUnchecked,
			Severity: schema.SeverityInfo,
			Message: "could not check whether runc pods share nodes with sandboxed pods: " + sb.Reason +
				". The rest of the preflight is unaffected",
			Remediation: "Grant list on core/pods (deploy/clusterrole-readonly.yaml already does) and re-run.",
		}}
	}

	var out []schema.PreflightFinding

	var shared []schema.SandboxNode
	runcTotal, credsOnShared := 0, 0
	for _, n := range sb.Nodes {
		if len(n.RuncWorkloadPods) == 0 {
			continue
		}
		shared = append(shared, n)
		runcTotal += len(n.RuncWorkloadPods)
		credsOnShared += len(n.SandboxedPodsWithAWSCredentials)
	}
	if len(shared) > 0 {
		parts := make([]string, 0, maxNamesInMessage)
		for i, n := range shared {
			if i == maxNamesInMessage {
				parts = append(parts, fmt.Sprintf("and %d more", len(shared)-maxNamesInMessage))
				break
			}
			parts = append(parts, fmt.Sprintf("%s (%d runc, %d sandboxed, %d with AWS credentials)",
				n.Name, len(n.RuncWorkloadPods), n.SandboxedPods, len(n.SandboxedPodsWithAWSCredentials)))
		}
		out = append(out, schema.PreflightFinding{
			ID:       FindingSandboxNodeRuncColocation,
			Severity: schema.SeverityWarn,
			Message: fmt.Sprintf("%d of %d nodes running sandboxed pods also run %d runc workload pods (DaemonSet pods not counted): %s. "+
				"A container escape from any of those runc pods is node root, and node root can reach the credentials of every pod on the node, "+
				"including the %d sandboxed pods there that hold AWS credentials",
				len(shared), len(sb.Nodes), runcTotal, strings.Join(parts, "; "), credsOnShared),
			Remediation: "Dedicate the sandbox nodes to sandboxed pods: taint the node group (the shipped gVisor RuntimeClass tolerates runtime=gvisor:NoSchedule; " +
				"for EKS Nitro Sandbox nodes, add a taint of your own and the matching toleration to the sandboxed pod templates), then move the listed runc pods off. " +
				"Keep DaemonSets on those nodes to the ones the node needs. The facts list every pod by name.",
		})
	}

	var withCreds []string
	for _, n := range sb.Nodes {
		withCreds = append(withCreds, n.SandboxedPodsWithAWSCredentials...)
	}
	if len(withCreds) > 0 {
		sort.Strings(withCreds)
		shown := withCreds
		suffix := ""
		if len(shown) > maxNamesInMessage {
			suffix = fmt.Sprintf(" and %d more", len(shown)-maxNamesInMessage)
			shown = shown[:maxNamesInMessage]
		}
		out = append(out, schema.PreflightFinding{
			ID:       FindingSandboxPodAWSCredentials,
			Severity: schema.SeverityInfo,
			Message: fmt.Sprintf("%d sandboxed pods get AWS credentials from EKS Pod Identity or IRSA: %s%s. "+
				"The sandbox does not hide these from the node: treat each role as available to anyone with root on the node. "+
				"agentmoat does not read IAM, so it cannot tell how broad the roles are",
				len(withCreds), strings.Join(shown, ", "), suffix),
			Remediation: "Give each sandboxed workload its own least-privilege role (for example, generate one from CloudTrail with IAM Access Analyzer), " +
				"and restrict who can create pods that name a sandbox RuntimeClass, since the pod spec decides which role the pod gets.",
		})
	}
	return out
}
