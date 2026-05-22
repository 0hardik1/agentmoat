// Package scanner: enumeration logic.
//
// Why we enumerate controllers (and not Pods)
//
//	A naive scanner would simply List() every Pod in the cluster and emit
//	one Workload per Pod. That is wrong in two ways. First, it
//	double-counts: a 20-replica Deployment becomes 20 rows, each with the
//	same PodSpec, and the operator has to dedupe by name prefix. Second,
//	and more important, the unit of *migration* is the controller, not the
//	Pod: an operator switches a workload to gVisor by patching the
//	controller's pod template (`spec.template.spec.runtimeClassName`), not
//	by editing individual Pods (those get recreated). So the scanner
//	reports one row per workload owner and the planner/applier patch that
//	owner.
//
// De-duplication precedence
//
//	Controllers can themselves own other controllers. We surface only the
//	*top-level* owner using this precedence (highest first):
//
//	  Deployment > StatefulSet > DaemonSet > CronJob > Job > Pod
//
//	Concretely:
//	  - A Deployment owns a ReplicaSet (we ignore ReplicaSets entirely)
//	    which owns Pods. We list the Deployment, skip its Pods.
//	  - A CronJob owns Jobs (one per fire) which own Pods. We list the
//	    CronJob, skip its Jobs, skip its Pods.
//	  - A Job that is NOT owned by a CronJob (a one-shot job submitted
//	    directly by an operator) is reported as a Job.
//	  - A Pod that is NOT owned by any of {ReplicaSet, StatefulSet,
//	    DaemonSet, Job} via a Controller=true OwnerReference is a
//	    standalone Pod and reported as such.
//
// Read-only
//
//	Enumerate performs only List() API calls. It never writes, patches, or
//	deletes anything. This file (and this package) is safe to invoke
//	against production clusters at any time.
package scanner

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// defaultPageSize is the number of objects requested per List() call when
// the caller did not set EnumerateOptions.PageSize. 500 mirrors what kubectl
// uses for chunked listing.
const defaultPageSize int64 = 500

// systemNamespaces is the explicit deny-list of reserved namespaces that
// Enumerate skips by default. Additionally any namespace whose name starts
// with "kube-" is skipped (this catches kube-flannel, kube-ovn, etc., that
// distributions add without coordinating with us). Operators who do want to
// scan these can pass EnumerateOptions.IncludeSystem=true.
var systemNamespaces = map[string]struct{}{
	"kube-system":     {},
	"kube-public":     {},
	"kube-node-lease": {},
	"kube-flannel":    {},
}

// Enumerate walks the cluster and returns one Workload per workload-owner
// controller (or per standalone Pod). See the package doc for the
// de-duplication rules.
//
// Errors from individual List() calls are wrapped with the kind name so the
// user can tell which API surface failed.
func Enumerate(ctx context.Context, client kubernetes.Interface, opts EnumerateOptions) ([]Workload, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	// Determine which namespaces to query. An empty slice means
	// cluster-wide (passed as "" to client-go's namespaced lister).
	namespaces := opts.Namespaces
	// When the caller did not name any namespaces, we issue cluster-wide
	// List() calls (one per kind) and filter the system namespaces in-Go
	// after the fact. This is cheaper than enumerating namespaces first.
	cluster := len(namespaces) == 0

	out := make([]Workload, 0, 64)

	// 1) Deployments
	deployments, err := listDeployments(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing Deployments: %w", err)
	}
	out = appendDeployments(out, deployments, cluster, opts.IncludeSystem)

	// 2) StatefulSets
	statefulSets, err := listStatefulSets(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing StatefulSets: %w", err)
	}
	out = appendStatefulSets(out, statefulSets, cluster, opts.IncludeSystem)

	// 3) DaemonSets
	daemonSets, err := listDaemonSets(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing DaemonSets: %w", err)
	}
	out = appendDaemonSets(out, daemonSets, cluster, opts.IncludeSystem)

	// 4) CronJobs (parents). We do these before Jobs so we know which Jobs
	// to skip below.
	cronJobs, err := listCronJobs(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing CronJobs: %w", err)
	}
	out = appendCronJobs(out, cronJobs, cluster, opts.IncludeSystem)

	// 5) Jobs. Skip any Job whose Controller=true OwnerReference points at
	// a CronJob: those are CronJob children and already accounted for.
	jobs, err := listJobs(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing Jobs: %w", err)
	}
	out = appendJobs(out, jobs, cluster, opts.IncludeSystem)

	// 6) Pods. Skip any Pod owned by a ReplicaSet, StatefulSet, DaemonSet,
	// or Job: those are already covered by the controller rows above.
	pods, err := listPods(ctx, client, namespaces, opts.LabelSelector, pageSize)
	if err != nil {
		return nil, fmt.Errorf("listing Pods: %w", err)
	}
	out = appendPods(out, pods, cluster, opts.IncludeSystem)

	sortWorkloads(out)
	return out, nil
}

// shouldSkipSystem returns true when a cluster-wide list should skip this
// namespace because it is a system namespace and the caller did not opt in.
func shouldSkipSystem(ns string, cluster, includeSystem bool) bool {
	return cluster && !includeSystem && isSystemNamespace(ns)
}

func appendDeployments(out []Workload, items []appsv1.Deployment, cluster, includeSystem bool) []Workload {
	for i := range items {
		d := &items[i]
		if shouldSkipSystem(d.Namespace, cluster, includeSystem) {
			continue
		}
		out = append(out, workloadFromDeployment(d))
	}
	return out
}

func appendStatefulSets(out []Workload, items []appsv1.StatefulSet, cluster, includeSystem bool) []Workload {
	for i := range items {
		s := &items[i]
		if shouldSkipSystem(s.Namespace, cluster, includeSystem) {
			continue
		}
		out = append(out, workloadFromStatefulSet(s))
	}
	return out
}

func appendDaemonSets(out []Workload, items []appsv1.DaemonSet, cluster, includeSystem bool) []Workload {
	for i := range items {
		ds := &items[i]
		if shouldSkipSystem(ds.Namespace, cluster, includeSystem) {
			continue
		}
		out = append(out, workloadFromDaemonSet(ds))
	}
	return out
}

func appendCronJobs(out []Workload, items []batchv1.CronJob, cluster, includeSystem bool) []Workload {
	for i := range items {
		cj := &items[i]
		if shouldSkipSystem(cj.Namespace, cluster, includeSystem) {
			continue
		}
		out = append(out, workloadFromCronJob(cj))
	}
	return out
}

func appendJobs(out []Workload, items []batchv1.Job, cluster, includeSystem bool) []Workload {
	for i := range items {
		j := &items[i]
		if shouldSkipSystem(j.Namespace, cluster, includeSystem) {
			continue
		}
		if hasControllerOfKind(j.OwnerReferences, "CronJob") {
			continue
		}
		out = append(out, workloadFromJob(j))
	}
	return out
}

func appendPods(out []Workload, items []corev1.Pod, cluster, includeSystem bool) []Workload {
	for i := range items {
		p := &items[i]
		if shouldSkipSystem(p.Namespace, cluster, includeSystem) {
			continue
		}
		if hasControllerOfKind(p.OwnerReferences,
			"ReplicaSet", "StatefulSet", "DaemonSet", "Job") {
			continue
		}
		out = append(out, workloadFromPod(p))
	}
	return out
}

// sortWorkloads applies the deterministic ordering used in the report so
// JSON/YAML diffs across runs are stable.
func sortWorkloads(workloads []Workload) {
	sort.SliceStable(workloads, func(i, j int) bool {
		if workloads[i].Namespace != workloads[j].Namespace {
			return workloads[i].Namespace < workloads[j].Namespace
		}
		if workloads[i].Kind != workloads[j].Kind {
			return workloads[i].Kind < workloads[j].Kind
		}
		return workloads[i].Name < workloads[j].Name
	})
}

// isSystemNamespace returns true if the namespace is one of the reserved
// system namespaces (or starts with the "kube-" prefix). Used to filter
// cluster-wide listings when EnumerateOptions.IncludeSystem is false.
func isSystemNamespace(ns string) bool {
	if _, ok := systemNamespaces[ns]; ok {
		return true
	}
	// Catch distribution-specific reserved namespaces (kube-flannel,
	// kube-ovn, kube-router, ...). Operators who care about these can
	// pass IncludeSystem=true.
	if strings.HasPrefix(ns, "kube-") {
		return true
	}
	return false
}

// hasControllerOfKind returns true if owners contains a Controller=true
// OwnerReference whose Kind is in the given set. Used to suppress workloads
// that are already represented by a higher-level controller.
func hasControllerOfKind(owners []metav1.OwnerReference, kinds ...string) bool {
	for _, o := range owners {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		for _, k := range kinds {
			if o.Kind == k {
				return true
			}
		}
	}
	return false
}

// imageRefsOf flattens the image references from init + main containers
// into one slice. Order: init containers first (in declared order), then
// main containers (in declared order). Ephemeral containers are skipped:
// they are a debug surface and not part of the workload definition.
func imageRefsOf(spec corev1.PodSpec) []string {
	refs := make([]string, 0, len(spec.InitContainers)+len(spec.Containers))
	for _, c := range spec.InitContainers {
		refs = append(refs, c.Image)
	}
	for _, c := range spec.Containers {
		refs = append(refs, c.Image)
	}
	return refs
}

// ---------------------------------------------------------------------------
// Per-kind List() helpers.
//
// Each follows the same chunked-list pattern: start with an empty Continue
// token, request `limit` objects, append the page, repeat until Continue is
// empty. We deliberately repeat the loop rather than hide it behind a
// generic helper: client-go's typed listers do not satisfy a single
// interface, so the generic version ends up with as much boilerplate as
// just inlining it.
// ---------------------------------------------------------------------------

func listDeployments(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]appsv1.Deployment, error) {
	var out []appsv1.Deployment
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

func listStatefulSets(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]appsv1.StatefulSet, error) {
	var out []appsv1.StatefulSet
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

func listDaemonSets(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]appsv1.DaemonSet, error) {
	var out []appsv1.DaemonSet
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

func listCronJobs(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]batchv1.CronJob, error) {
	var out []batchv1.CronJob
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

func listJobs(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]batchv1.Job, error) {
	var out []batchv1.Job
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

func listPods(ctx context.Context, client kubernetes.Interface, namespaces []string, labelSelector string, limit int64) ([]corev1.Pod, error) {
	var out []corev1.Pod
	scopes := listScopes(namespaces)
	for _, ns := range scopes {
		var cont string
		for {
			page, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
				Limit:         limit,
				Continue:      cont,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			if page.Continue == "" {
				break
			}
			cont = page.Continue
		}
	}
	return out, nil
}

// listScopes returns the slice of namespaces to iterate over for a single
// kind. An empty input means cluster-wide, which client-go expresses as a
// single "" namespace argument. A non-empty input is returned as-is so we
// loop over the caller's explicit list.
func listScopes(namespaces []string) []string {
	if len(namespaces) == 0 {
		return []string{""}
	}
	return namespaces
}

// ---------------------------------------------------------------------------
// Conversion helpers: K8s object -> Workload.
// Each unwraps the PodSpec from the kind-specific nesting and copies the
// controller's metadata. We avoid sharing references to map fields with the
// caller's API objects because client-go objects are not safe to mutate.
// ---------------------------------------------------------------------------

func workloadFromDeployment(d *appsv1.Deployment) Workload {
	spec := d.Spec.Template.Spec
	return Workload{
		Kind:        "Deployment",
		Namespace:   d.Namespace,
		Name:        d.Name,
		PodSpec:     spec,
		Labels:      copyMap(d.Labels),
		Annotations: copyMap(d.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

func workloadFromStatefulSet(s *appsv1.StatefulSet) Workload {
	spec := s.Spec.Template.Spec
	return Workload{
		Kind:        "StatefulSet",
		Namespace:   s.Namespace,
		Name:        s.Name,
		PodSpec:     spec,
		Labels:      copyMap(s.Labels),
		Annotations: copyMap(s.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

func workloadFromDaemonSet(ds *appsv1.DaemonSet) Workload {
	spec := ds.Spec.Template.Spec
	return Workload{
		Kind:        "DaemonSet",
		Namespace:   ds.Namespace,
		Name:        ds.Name,
		PodSpec:     spec,
		Labels:      copyMap(ds.Labels),
		Annotations: copyMap(ds.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

func workloadFromCronJob(cj *batchv1.CronJob) Workload {
	// CronJob nests two layers deep: spec.jobTemplate.spec.template.spec.
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	return Workload{
		Kind:        "CronJob",
		Namespace:   cj.Namespace,
		Name:        cj.Name,
		PodSpec:     spec,
		Labels:      copyMap(cj.Labels),
		Annotations: copyMap(cj.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

func workloadFromJob(j *batchv1.Job) Workload {
	spec := j.Spec.Template.Spec
	return Workload{
		Kind:        "Job",
		Namespace:   j.Namespace,
		Name:        j.Name,
		PodSpec:     spec,
		Labels:      copyMap(j.Labels),
		Annotations: copyMap(j.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

func workloadFromPod(p *corev1.Pod) Workload {
	spec := p.Spec
	return Workload{
		Kind:        "Pod",
		Namespace:   p.Namespace,
		Name:        p.Name,
		PodSpec:     spec,
		Labels:      copyMap(p.Labels),
		Annotations: copyMap(p.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
}

// copyMap returns a shallow copy of m. We never share map references with
// the API objects returned by client-go: downstream code (the classifier,
// planner, etc.) should be able to read these maps without worrying about
// concurrent informer mutations. Returns nil for a nil input.
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
