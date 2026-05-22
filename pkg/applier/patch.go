// Package applier: strategic-merge / JSON-merge patch generation.
//
// What this file is responsible for
//
//   - Producing the patch bytes the applier sends to the K8s API for a given
//     PlanStep + controller kind.
//   - Knowing the kind-specific JSON path for the pod template:
//
//       Pod                                          spec.runtimeClassName
//       Deployment, StatefulSet, DaemonSet, Job      spec.template.spec.runtimeClassName
//       CronJob                                      spec.jobTemplate.spec.template.spec.runtimeClassName
//
//   - Choosing a patch type per direction:
//
//       Apply    -> Strategic Merge Patch. Tolerations merge by key (the
//                   strategic-merge-key for v1.Toleration is "key"), so we
//                   can add ours without duplicating an existing entry.
//       Rollback -> JSON Merge Patch (RFC 7396). Setting
//                   `runtimeClassName: null` deletes the field. We do not
//                   remove the toleration on rollback: an untainted node
//                   does not select on it, so it is harmless. Removing
//                   the *specific* toleration via JSON Patch indices is
//                   fragile because the list may have been re-ordered by
//                   other admission controllers since apply.
//
// Why patches and not full-object updates
//
//   Patches are O(small) on the wire and avoid optimistic-concurrency
//   conflicts on unrelated fields. A controller-owner mutating a pod
//   template via .Update() racing with HPA, the deployment controller, or
//   another admission webhook is a recipe for a 409. A targeted patch is
//   the kubectl-style way and what every other migration tool does.
package applier

import (
	"encoding/json"
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PatchKind is one of the K8s kinds the applier knows how to patch.
// Helper constants kept package-private; callers refer to them via
// schema.PlanStep.Target.Kind (a free-form string from upstream).
const (
	kindPod         = "Pod"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindJob         = "Job"
	kindCronJob     = "CronJob"
)

// IsSupportedKind returns true if the applier knows how to patch this kind.
// Exposed for the orchestrator's pre-flight check.
func IsSupportedKind(kind string) bool {
	switch kind {
	case kindPod, kindDeployment, kindStatefulSet, kindDaemonSet, kindJob, kindCronJob:
		return true
	}
	return false
}

// applyPatchBytes builds the strategic-merge-patch JSON the applier sends
// for an "apply" operation. The returned bytes set runtimeClassName on the
// kind-correct path and (when AddToleration is true) add the
// `runtime=gvisor:NoSchedule` toleration via the strategic-merge "merge by
// key" rule.
//
// The function is pure: same step -> same bytes. Snapshot tests in
// patch_test.go pin the exact output.
func applyPatchBytes(step schema.PlanStep) ([]byte, types.PatchType, error) {
	if step.RuntimeClassName == "" {
		return nil, "", fmt.Errorf("applier: PlanStep has empty RuntimeClassName")
	}
	if !IsSupportedKind(step.Target.Kind) {
		return nil, "", fmt.Errorf("applier: unsupported kind %q", step.Target.Kind)
	}

	// podSpecPatch is the change at the leaf-most level (a PodSpec). The
	// surrounding wrapper differs by Kind (built below).
	leaf := map[string]any{
		"runtimeClassName": step.RuntimeClassName,
	}
	if step.AddToleration {
		// Strategic merge: tolerations have patchMergeKey "key", so this
		// is additive: an existing toleration with key="runtime" gets
		// overwritten (which is what we want), other tolerations stay.
		leaf["tolerations"] = []corev1.Toleration{gvisorToleration()}
	}

	patch := wrapLeafForKind(step.Target.Kind, leaf)

	out, err := json.Marshal(patch)
	if err != nil {
		return nil, "", fmt.Errorf("applier: marshalling apply patch: %w", err)
	}
	return out, types.StrategicMergePatchType, nil
}

// rollbackPatchBytes builds the JSON merge patch the applier sends for a
// "rollback" operation. Setting runtimeClassName to nil under the
// kind-correct path removes it. The toleration is intentionally left in
// place (see file-level comment).
func rollbackPatchBytes(step schema.PlanStep) ([]byte, types.PatchType, error) {
	if !IsSupportedKind(step.Target.Kind) {
		return nil, "", fmt.Errorf("applier: unsupported kind %q", step.Target.Kind)
	}

	leaf := map[string]any{
		"runtimeClassName": nil,
	}
	patch := wrapLeafForKind(step.Target.Kind, leaf)

	out, err := json.Marshal(patch)
	if err != nil {
		return nil, "", fmt.Errorf("applier: marshalling rollback patch: %w", err)
	}
	return out, types.MergePatchType, nil
}

// wrapLeafForKind nests the leaf-most PodSpec patch under the kind-correct
// JSON-path wrapper. Kept here so the kind-to-path mapping has exactly one
// definition.
func wrapLeafForKind(kind string, leaf map[string]any) map[string]any {
	switch kind {
	case kindPod:
		return map[string]any{
			"spec": leaf,
		}
	case kindDeployment, kindStatefulSet, kindDaemonSet, kindJob:
		return map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": leaf,
				},
			},
		}
	case kindCronJob:
		return map[string]any{
			"spec": map[string]any{
				"jobTemplate": map[string]any{
					"spec": map[string]any{
						"template": map[string]any{
							"spec": leaf,
						},
					},
				},
			},
		}
	default:
		// Should be unreachable; the caller already checked IsSupportedKind.
		return map[string]any{}
	}
}

// gvisorToleration is the toleration agentmoat injects so the migrated pod
// can land on a node that carries the matching taint. Plan.md section 9.4
// pairs the taint `runtime=gvisor:NoSchedule` with this toleration.
func gvisorToleration() corev1.Toleration {
	return corev1.Toleration{
		Key:      schema.GVisorTaintKey,
		Operator: corev1.TolerationOpEqual,
		Value:    schema.GVisorTaintValue,
		Effect:   corev1.TaintEffect(schema.GVisorTaintEffect),
	}
}
