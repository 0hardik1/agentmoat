// Package agentmoat: the Kubernetes client, built one way.
//
// Every orchestrator function that reads the cluster needs a client. Tests
// (and the MCP server's test harness) inject a fake one through the
// KubeClient field of the options struct; production callers leave that
// field nil and get a real client from the kubeconfig loader.
//
// Keeping this in one helper has a second purpose: it is the single place
// where a cluster client comes into existence for the read path. The code
// paths that must work with no cluster at all (manifest input) are easy to
// audit, because they do not call it.
package agentmoat

import (
	"fmt"

	"github.com/0hardik1/agentmoat/internal/kube"
	"k8s.io/client-go/kubernetes"
)

// newKubeClient returns injected when it is not nil. Otherwise it builds a
// client from the kubeconfig (explicit path, then KUBECONFIG, then
// ~/.kube/config) or from the in-cluster service account. See
// internal/kube for the search order.
func newKubeClient(injected kubernetes.Interface, kubeconfigPath, kubeContext string) (kubernetes.Interface, error) {
	if injected != nil {
		return injected, nil
	}
	cs, _, err := kube.NewClient(kubeconfigPath, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return cs, nil
}
