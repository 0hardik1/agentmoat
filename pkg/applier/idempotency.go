// Package applier: the marks on a namespace.
//
// The applier keeps its state in the cluster, as two annotations on each
// namespace that a plan touches. The value of each is the hash of a plan
// (MigrationPlan.Metadata.PlanHash, a SHA-256 in hex).
//
//	agentmoat.io/plan-hash      "this plan is applied here: every step is done"
//	agentmoat.io/apply-started  "an apply of this plan started here"
//
// Why two
//
//   - plan-hash is the idempotency stamp. A second apply of the same plan
//     finds it and reports every step as already-applied. So it can be
//     written only after every step in the namespace is done: a namespace
//     with one step that failed must not look done, or the second apply
//     would not retry.
//
//   - That rule leaves a gap for the rollback. An apply that did not finish
//     (one patch failed, the process was stopped between two patches, the
//     stamp write failed) changed workloads and left no stamp. A rollback
//     that looks only at the stamp finds nothing and undoes nothing.
//
//   - So apply writes apply-started BEFORE its first patch in a namespace:
//     record first, then mutate. If that write fails, apply changes nothing
//     in the namespace. From then on the cluster always says that this plan
//     touched the namespace, whatever happens to the process.
//
// The life of the two marks
//
//	apply, before the first patch in a namespace   apply-started = hash, and the
//	                                               stamp of an older plan removed
//	apply, every step in the namespace done        plan-hash = hash, apply-started removed
//	apply, a step failed                           apply-started stays, no plan-hash
//	rollback, every step undone                    the marks of this plan removed
//
// A rollback acts on a namespace only when one of the two marks carries the
// hash of its plan. It never decides from the workloads alone: a class on a
// workload can be from another tool, and a rollback of a plan that was never
// applied must change nothing.
//
// # One plan governs a namespace at a time
//
// The two marks are one slot, and the newest apply has it.
//
//   - An apply takes the namespace over. The write of apply-started also
//     removes the stamp of an older plan, in the same patch. If the stamp
//     stayed, it could become false: this apply can fail, its rollback then
//     removes the class from workloads that the older plan also lists, and
//     the old stamp would still say "applied". A second apply of the older
//     plan would then report already-applied for workloads with no class.
//   - A rollback removes only the marks of its own plan. A namespace whose
//     marks have another hash belongs to another plan: its steps are skipped
//     and its marks stay.
//
// # Every plan has a hash
//
// The marks are the hash of the plan, so a plan with no hash cannot be
// recorded, and a change with no record is one that a rollback cannot find.
// CheckPlan refuses such a plan for apply and for rollback, dry-run too.
// `agentmoat plan` always sets the hash.
package applier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// namespaceMarks are the two annotations of one namespace. An empty string
// is "not set".
type namespaceMarks struct {
	// stamp is the value of agentmoat.io/plan-hash.
	stamp string
	// started is the value of agentmoat.io/apply-started.
	started string
}

// markState is what the marks of a namespace mean for a rollback with one
// plan.
type markState int

const (
	// marksNone: the namespace has no mark. No apply is recorded here.
	marksNone markState = iota
	// marksOther: the namespace has a mark, and none with the hash of this
	// plan. Another plan governs it.
	marksOther
	// marksStamped: the stamp has the hash of this plan. Every step in the
	// namespace was applied.
	marksStamped
	// marksStarted: only apply-started has the hash of this plan. The
	// apply did not finish: a step can be applied or not.
	marksStarted
)

// stateFor says what the marks mean for a plan with this hash. The hash is
// never empty here (CheckPlan).
//
// The stamp decides before apply-started. This version does not leave the
// two marks with different hashes on one namespace, but an older version or
// a person can, so each combination has an answer.
func (m namespaceMarks) stateFor(hash string) markState {
	switch {
	case m.stamp == "" && m.started == "":
		return marksNone
	case m.stamp == hash:
		return marksStamped
	case m.started == hash:
		return marksStarted
	default:
		return marksOther
	}
}

// errNoPlanHash is the refusal of a plan that cannot be recorded.
var errNoPlanHash = errors.New("the plan has no metadata.planHash. apply and rollback record a plan on the cluster by its hash; " +
	"with no hash, a change would have no record and a rollback could not find it. Make the plan with 'agentmoat plan'")

// CheckPlan says if a plan can be applied or rolled back at all. It reads no
// cluster, so a caller can refuse a plan before it builds a client. Apply
// and Rollback call it too.
func CheckPlan(plan *schema.MigrationPlan) error {
	if plan == nil {
		return errors.New("applier: opts.Plan is nil")
	}
	if plan.Metadata.PlanHash == "" {
		return errNoPlanHash
	}
	return nil
}

// readNamespaceMarks reads the two annotations of a namespace.
//
// All client errors (including NotFound for a missing namespace) are
// propagated; the caller (readAllNamespaceMarks) is what maps NotFound to
// "no mark".
func readNamespaceMarks(ctx context.Context, client kubernetes.Interface, ns string) (namespaceMarks, error) {
	got, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return namespaceMarks{}, fmt.Errorf("getting namespace %q: %w", ns, err)
	}
	return namespaceMarks{
		stamp:   got.Annotations[schema.PlanHashAnnotation],
		started: got.Annotations[schema.ApplyStartedAnnotation],
	}, nil
}

// readAllNamespaceMarks reads the marks of every namespace that the plan
// touches. A namespace that does not exist has no mark. Any other error
// stops the run before the first change.
func readAllNamespaceMarks(ctx context.Context, client kubernetes.Interface, plan *schema.MigrationPlan) (map[string]namespaceMarks, error) {
	out := make(map[string]namespaceMarks)
	for _, ns := range affectedNamespaces(plan) {
		marks, err := readNamespaceMarks(ctx, client, ns)
		if err != nil {
			if apierrors.IsNotFound(err) {
				out[ns] = namespaceMarks{}
				continue
			}
			return nil, err
		}
		out[ns] = marks
	}
	return out, nil
}

// patchNamespaceMarks sets and removes annotations of a namespace in one
// JSON merge patch: a value sets the key, nil removes it (JSON null). One
// patch for two keys means that the namespace is never seen with the stamp
// and apply-started together, or with none of them, in the middle of a
// change.
func patchNamespaceMarks(ctx context.Context, client kubernetes.Interface, ns string, changes map[string]*string) error {
	annotations := make(map[string]any, len(changes))
	for key, value := range changes {
		if value == nil {
			annotations[key] = nil // delete
		} else {
			annotations[key] = *value
		}
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
	})
	if err != nil {
		return fmt.Errorf("marshaling namespace annotation patch: %w", err)
	}
	if _, err := client.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching namespace %q annotations: %w", ns, err)
	}
	return nil
}

// startRecorder writes apply-started on a namespace before the first patch
// there. It writes one time for each namespace and keeps the result, so the
// second step in a namespace does not write again, and every step in a
// namespace where the write failed gets the same error.
type startRecorder struct {
	client kubernetes.Interface
	hash   string
	marks  map[string]namespaceMarks
	done   map[string]error

	// stderr gets one line when this apply replaces the record of another
	// plan. After the write, nothing else in the cluster has that hash.
	stderr io.Writer
}

func newStartRecorder(client kubernetes.Interface, hash string, marks map[string]namespaceMarks, stderr io.Writer) *startRecorder {
	return &startRecorder{client: client, hash: hash, marks: marks, done: map[string]error{}, stderr: stderr}
}

// record makes sure that the namespace says "an apply of this plan started
// here", and that it does not also say "an older plan is applied here". A
// nil return means that the record is there, and the caller can patch.
//
// The caller comes here only when the stamp is not the hash of this plan (a
// namespace with that stamp is already applied, and gets no patch). So a
// stamp that is there belongs to an older plan, and this write removes it:
// see "One plan governs a namespace at a time" at the top of this file.
func (r *startRecorder) record(ctx context.Context, ns string) error {
	if err, ok := r.done[ns]; ok {
		return err
	}
	var err error
	marks := r.marks[ns]
	if marks.started != r.hash || marks.stamp != "" {
		changes := map[string]*string{schema.ApplyStartedAnnotation: &r.hash}
		if marks.stamp != "" {
			changes[schema.PlanHashAnnotation] = nil
		}
		err = patchNamespaceMarks(ctx, r.client, ns, changes)
		if replaced := marks.otherThan(r.hash); err == nil && replaced != "" {
			_, _ = fmt.Fprintf(r.stderr, "note: namespace %s had the record of another plan (%s). This apply replaces it. "+
				"To roll back that plan later, apply it again first.\n", ns, replaced)
		}
	}
	r.done[ns] = err
	return err
}

// otherThan names the marks that do not have this hash, for a message:
// "plan-hash abc, apply-started def". It is "" when no such mark is there.
func (m namespaceMarks) otherThan(hash string) string {
	var parts []string
	if m.stamp != "" && m.stamp != hash {
		parts = append(parts, "plan-hash "+m.stamp)
	}
	if m.started != "" && m.started != hash {
		parts = append(parts, "apply-started "+m.started)
	}
	return strings.Join(parts, ", ")
}

// stampNamespace records a complete apply: the stamp gets the hash, and
// apply-started goes.
func stampNamespace(ctx context.Context, client kubernetes.Interface, ns, hash string) error {
	return patchNamespaceMarks(ctx, client, ns, map[string]*string{
		schema.PlanHashAnnotation:     &hash,
		schema.ApplyStartedAnnotation: nil,
	})
}

// clearOwnMarks records a complete rollback: it removes the marks that
// belong to this plan, and no other. It returns nil with no API call when
// the namespace has no mark of this plan.
func clearOwnMarks(ctx context.Context, client kubernetes.Interface, ns, hash string, marks namespaceMarks) error {
	changes := map[string]*string{}
	if marks.stamp == hash {
		changes[schema.PlanHashAnnotation] = nil
	}
	if marks.started == hash {
		changes[schema.ApplyStartedAnnotation] = nil
	}
	if len(changes) == 0 {
		return nil
	}
	return patchNamespaceMarks(ctx, client, ns, changes)
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
