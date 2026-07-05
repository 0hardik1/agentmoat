// Package applier: idempotency helpers.
//
// Plan.md section 12.1 says: "Re-running an applied plan reports
// `already-applied` and exits `0`. State is stored as a
// `agentmoat.io/plan-hash` annotation on the namespace."
//
// This file owns:
//   - reading the `agentmoat.io/plan-hash` annotation from a namespace,
//   - writing or clearing it,
//   - and the small "do we still need to mutate?" decision the applier
//     consults before each step.
//
// One annotation per namespace. The annotation value is the MigrationPlan's
// PlanHash (a SHA-256 hex digest). The annotation is written *after* every
// step in the namespace has been applied (or skipped); a partial apply
// leaves the annotation unchanged so a follow-up apply does the right thing.
package applier

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// readNamespaceAnnotation fetches the value of the agentmoat.io/plan-hash
// annotation on the given namespace, returning "" if the annotation is
// absent.
//
// All client errors (including NotFound for a missing namespace) are
// propagated; the caller (readAllNamespaceAnnotations) is what maps
// NotFound to "no annotation".
func readNamespaceAnnotation(ctx context.Context, client kubernetes.Interface, ns string) (string, error) {
	got, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting namespace %q: %w", ns, err)
	}
	if got.Annotations == nil {
		return "", nil
	}
	return got.Annotations[schema.PlanHashAnnotation], nil
}

// writeNamespaceAnnotation sets (or, when planHash == "", clears) the
// agentmoat.io/plan-hash annotation on the given namespace via a JSON
// merge patch. Clearing uses JSON null to delete the key.
//
// Returns the patch bytes that would be (or were) sent, so the audit log
// and dry-run output can echo them.
func writeNamespaceAnnotation(ctx context.Context, client kubernetes.Interface, ns, planHash string, dryRun bool) ([]byte, error) {
	annotations := map[string]any{}
	if planHash == "" {
		annotations[schema.PlanHashAnnotation] = nil // delete
	} else {
		annotations[schema.PlanHashAnnotation] = planHash
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshaling namespace annotation patch: %w", err)
	}
	if dryRun {
		return body, nil
	}
	opts := metav1.PatchOptions{}
	if _, err := client.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, body, opts); err != nil {
		return body, fmt.Errorf("patching namespace %q annotation: %w", ns, err)
	}
	return body, nil
}

// affectedNamespaces returns the deterministic, deduplicated list of
// namespaces a MigrationPlan touches. The applier uses this both to
// pre-flight read the annotation and to write it back after a successful
// apply.
func affectedNamespaces(plan *schema.MigrationPlan) []string {
	seen := make(map[string]struct{}, 4)
	var out []string
	for _, s := range plan.Spec.Steps {
		if _, ok := seen[s.Target.Namespace]; ok {
			continue
		}
		seen[s.Target.Namespace] = struct{}{}
		out = append(out, s.Target.Namespace)
	}
	return out
}
