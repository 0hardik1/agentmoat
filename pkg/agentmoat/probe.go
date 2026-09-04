// Package agentmoat: the Probe orchestrator.
//
// Probe runs pkg/probe: the preflight plus a one-shot pod that reads the
// runsc binary on a gVisor node and reports the NVIDIA host driver versions
// its nvproxy supports. It is the third verb that can create something in
// the cluster (after apply and rollback) and follows the same rule: the
// CLI and the MCP tool default DryRun to true.
//
// The result is a PreflightReport with Metadata.Probe and, after a real
// run, Spec.Facts.GPU.Nvproxy. Save it with --output json and hand it to
// `scan --facts` so the classifier can settle the gpu-passthrough verdict
// without the scan creating anything.
package agentmoat

import (
	"context"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/probe"
)

// Probe runs the nvproxy probe. A probe pod that could not complete is a
// finding in the report (nvproxy-probe-failed), not an error; only
// precondition failures (kubeconfig, RBAC, a leftover pod) return one.
func Probe(ctx context.Context, opts ProbeOptions) (*schema.PreflightReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("probe: building kubernetes client: %w", err)
		}
		client = cs
	}

	if opts.DryRun {
		_, _ = fmt.Fprintln(stderr, "probe: dry run, no pod will be created (pass --dry-run=false to run it)")
	}
	report, err := probe.Run(ctx, probe.Options{
		Client:           client,
		RuntimeClassName: opts.RuntimeClassName,
		Namespace:        opts.Namespace,
		Image:            opts.Image,
		RunscPath:        opts.RunscPath,
		Timeout:          opts.Timeout,
		DryRun:           opts.DryRun,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath, opts.Context),
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, err
	}

	pm := report.Metadata.Probe
	switch {
	case pm == nil:
	case pm.Succeeded:
		nv := report.Spec.Facts.GPU.Nvproxy
		_, _ = fmt.Fprintf(stderr, "probe: pod %s/%s on node %s read runsc %s with %d supported driver(s)\n",
			pm.Namespace, pm.PodName, pm.Node, nv.RunscVersion, len(nv.SupportedDrivers))
	case pm.DryRun && pm.Node != "":
		_, _ = fmt.Fprintf(stderr, "probe: would create pod %s/%s on node %s\n", pm.Namespace, pm.PodName, pm.Node)
	}
	sum := report.Spec.Summary
	_, _ = fmt.Fprintf(stderr, "preflight: RuntimeClass %q ready=%v (%d error, %d warn, %d info)\n",
		report.Metadata.RuntimeClassName, sum.Ready, sum.Error, sum.Warn, sum.Info)
	return report, nil
}
