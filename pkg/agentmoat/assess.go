// Package agentmoat: AssessWorkload orchestrator.
//
// AssessWorkload is the single-workload counterpart to Scan. The MCP
// server's `assess_workload` tool calls it so an operator (or an LLM
// acting on their behalf) can ask "is this one workload compatible?"
// without paying for a cluster-wide enumeration.
//
// Pipeline:
//  1. Build (or accept) a Kubernetes client.
//  2. Build the classifier registry (built-ins + optional YAML overrides).
//  3. Fetch the workload via scanner.GetByRef.
//  4. Resolve the cluster facts (best-effort node read, or --facts file)
//     and classify via classifier.ClassifyWithFacts.
//  5. Return the verdict as schema.WorkloadResult so the MCP tool body is
//     directly JSON-serializable.
//
// AssessWorkload is read-only and safe to call from any context with a
// read-only kubeconfig.
package agentmoat

import (
	"context"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// AssessWorkload classifies one named workload and returns the verdict as
// a schema.WorkloadResult. Returns a non-nil error for precondition
// failures (missing kind/namespace/name, kubeconfig unloadable, workload
// not found, rules YAML unreadable).
func AssessWorkload(ctx context.Context, opts AssessWorkloadOptions) (*schema.WorkloadResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	if opts.Kind == "" || opts.Namespace == "" || opts.Name == "" {
		return nil, fmt.Errorf("assess: kind, namespace, and name are required")
	}

	client, err := newKubeClient(opts.KubeClient, opts.KubeconfigPath, opts.Context)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
	}

	registry, err := buildRegistry(opts.RulesYAMLPath, stderr)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
	}

	workload, err := scanner.GetByRef(ctx, client, opts.Kind, opts.Namespace, opts.Name)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
	}

	// Same facts as scan, so a single-workload verdict never disagrees
	// with the cluster-wide one for the same object.
	facts, err := resolveClusterFacts(ctx, client, factsSource{
		RuntimeClassName: opts.RuntimeClassName, SkipClusterFacts: opts.SkipClusterFacts, FactsPath: opts.FactsPath,
	}, stderr)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
	}

	result := workloadResult(workload, classifier.ClassifyWithFacts(workload, registry, facts))
	return &result, nil
}
