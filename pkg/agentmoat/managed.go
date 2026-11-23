// Package agentmoat: deploy tool markers, and the plan warning from them.
//
// `apply` patches the live object. Many teams do not own their workloads
// that way: a tool (Argo CD, Flux, Helm) deploys them from a source, which
// is a git repository or a chart. A live change is then not in that source.
// What the tool does with such a change at its next deploy depends on the
// tool and on its settings, so this code makes no claim about that. It only
// tells the reader, before an apply, that the source needs the change too.
//
// The markers are the ones that each tool writes on the objects that it
// deploys. Each key below is from the source that its comment names:
//
//   - Argo CD: the annotation `argocd.argoproj.io/tracking-id` is the
//     default tracking method from Argo CD 3.0 (the upgrade notes for 3.0:
//     "The default behavior for tracking resources has changed to use
//     annotation-based tracking instead of label-based tracking"). The
//     label `argocd.argoproj.io/instance` is the example of a custom
//     instance label in the Argo CD documents. The label
//     `app.kubernetes.io/instance` is NOT read here, although it is the
//     default of Argo CD 2.x: the Argo CD documents say that Helm charts
//     and operators write it too, so it is not evidence of Argo CD. An
//     Argo CD 2.x install with its default settings thus leaves no marker
//     that this code reads.
//   - Flux: the kustomize-controller writes the labels
//     `kustomize.toolkit.fluxcd.io/name` and `.../namespace` (fluxcd/pkg,
//     ssa/manager.go, SetOwnerLabels). The helm-controller writes
//     `helm.toolkit.fluxcd.io/name` and `.../namespace` (helm-controller,
//     internal/postrender).
//   - Helm: the annotations `meta.helm.sh/release-name` and
//     `meta.helm.sh/release-namespace` ("Helm adds these annotations to
//     resources built from the release manifest").
//
// Order matters. Argo CD can deploy a Helm chart (it renders the chart and
// applies the objects; no Helm marker), and Flux installs a chart with Helm
// (Helm marker and Flux marker). The first tool in the order Argo CD, Flux,
// Helm is the one that owns the source, so it is the one to name.
//
// No marker is not proof of no owner. A pipeline that runs `kubectl apply`
// leaves nothing that this code can read, and each tool can be set to use
// another key. The warning says so.
package agentmoat

import (
	"fmt"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// WarningSourceManaged is the ID of the plan warning for steps whose
// workload has a deploy tool marker.
const WarningSourceManaged = "source-managed"

// marker is one key that a deploy tool writes on an object.
type marker struct {
	tool       string
	annotation bool // false: a label
	key        string
}

// markers is the list, in the order of precedence.
var markers = []marker{
	{schema.ManagedByArgoCD, true, "argocd.argoproj.io/tracking-id"},
	{schema.ManagedByArgoCD, false, "argocd.argoproj.io/instance"},
	{schema.ManagedByFlux, false, "kustomize.toolkit.fluxcd.io/name"},
	{schema.ManagedByFlux, false, "helm.toolkit.fluxcd.io/name"},
	{schema.ManagedByHelm, true, "meta.helm.sh/release-name"},
}

// managedBy returns the deploy tool whose marker is on an object with these
// labels and annotations, or nil. It is a pure function. The evidence is the
// key and never the value: a value is text from the cluster or from a
// manifest, and a key in this list is a constant.
func managedBy(labels, annotations map[string]string) *schema.ManagedBy {
	for _, m := range markers {
		from, where := labels, "label"
		if m.annotation {
			from, where = annotations, "annotation"
		}
		if _, ok := from[m.key]; ok {
			return &schema.ManagedBy{Tool: m.tool, Evidence: where + " " + m.key}
		}
	}
	return nil
}

// toolNames are the names of the tools for a sentence, in the order that the
// warning lists them.
var toolNames = []struct{ tool, name string }{
	{schema.ManagedByArgoCD, "Argo CD"},
	{schema.ManagedByFlux, "Flux"},
	{schema.ManagedByHelm, "Helm"},
}

// sourceManagedWarning returns the warning for a plan whose steps include
// workloads with a deploy tool marker, or nil when no step has one. It reads
// the marker from the report rows; a step has only the target.
//
// The text is the same for the same plan and report: the tools are in a
// fixed order, and there is no name of a workload in it (the report has
// them, in `managedBy`).
func sourceManagedWarning(plan *schema.MigrationPlan, report *schema.ScanReport) *schema.PlanWarning {
	byTarget := make(map[schema.WorkloadRef]string, len(report.Spec.Workloads))
	for _, w := range report.Spec.Workloads {
		if w.ManagedBy != nil {
			byTarget[schema.WorkloadRef{Kind: w.Kind, Namespace: w.Namespace, Name: w.Name}] = w.ManagedBy.Tool
		}
	}
	counts := map[string]int{}
	total := 0
	for _, step := range plan.Spec.Steps {
		if tool, ok := byTarget[step.Target]; ok {
			counts[tool]++
			total++
		}
	}
	if total == 0 {
		return nil
	}
	var parts []string
	for _, t := range toolNames {
		if n := counts[t.tool]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", t.name, n))
		}
	}
	return &schema.PlanWarning{
		ID: WarningSourceManaged,
		Message: fmt.Sprintf("%d of %d workload(s) in this plan have the marker of a deploy tool (%s). "+
			"'agentmoat apply' changes the live objects only: the source that the tool deploys from (a git repository, a chart) does not get the change. "+
			"Add runtimeClassName to that source too. What the tool does with a live change that is not in its source depends on the tool and its settings. "+
			"A workload with no marker can also have such a source",
			total, len(plan.Spec.Steps), strings.Join(parts, ", ")),
	}
}
