// Package agentmoat: the Preflight orchestrator.
//
// Preflight is the read-only "can this cluster run pods that request the
// RuntimeClass?" check. It is the same code path Apply runs before it
// touches a step, exposed as its own verb so an operator can run it on
// day zero, long before there is a plan to apply, and so CI can gate on
// exit 5 (docs/exit-codes.md).
//
// Unlike scan's best-effort facts (facts.go), Preflight fails on RBAC
// errors: it exists to read the nodes and the RuntimeClass, so "could not
// read them" is the answer, not something to degrade around.
package agentmoat

import (
	"context"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
)

// Preflight reads one RuntimeClass and the node list, evaluates them, and
// returns a PreflightReport. A not-ready cluster is a report with
// Spec.Summary.Ready=false, not an error; only precondition failures
// (kubeconfig, RBAC, network) return a non-nil error.
func Preflight(ctx context.Context, opts PreflightOptions) (*schema.PreflightReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("preflight: building kubernetes client: %w", err)
		}
		client = cs
	}

	report, err := preflight.Run(ctx, preflight.Options{
		Client:           client,
		RuntimeClassName: opts.RuntimeClassName,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath, opts.Context),
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, err
	}

	sum := report.Spec.Summary
	_, _ = fmt.Fprintf(stderr, "preflight: RuntimeClass %q ready=%v (%d error, %d warn, %d info)\n",
		report.Metadata.RuntimeClassName, sum.Ready, sum.Error, sum.Warn, sum.Info)
	return report, nil
}
