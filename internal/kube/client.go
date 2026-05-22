// Package kube wraps the small amount of client-go boilerplate agentmoat
// needs to talk to a Kubernetes API server. The rest of the codebase imports
// this package rather than pulling in client-go directly so that the
// kubeconfig resolution rules live in one place.
//
// The loader is intentionally three-tier and tries each tier in this order:
//
//  1. Explicit path. If the caller passes kubeconfigPath != "" (typically
//     from the CLI's --kubeconfig flag), we load that file. An optional
//     contextName argument overrides the current-context inside it. This
//     tier is used when an operator wants to point at a specific cluster
//     without exporting KUBECONFIG.
//
//  2. Standard search path. The loader honours the KUBECONFIG env var (a
//     colon-separated list, just like kubectl) and falls back to
//     ~/.kube/config. This is the most common path on a workstation.
//
//  3. In-cluster. If neither of the above yields a usable config, we try
//     rest.InClusterConfig(). This is what agentmoat uses when it is
//     deployed as a Pod inside the cluster it is meant to scan (Phase 6's
//     operator mode, plus any future CronJob deployments).
//
// Every failure case returns an error wrapped with fmt.Errorf(..., %w) so
// callers can errors.Is() against the underlying client-go sentinel, and the
// wrapping prefix tells the user which tier failed and why. We avoid raw
// client-go error strings (like "stat /foo: no such file or directory")
// because they bury the actionable bit (the path we tried) inside a generic
// syscall message.
package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// userAgent is sent on every API request so cluster admins can attribute
// traffic in audit logs. Keep this short and recognisable.
const userAgent = "agentmoat"

// Client-side rate limits. The defaults baked into client-go (QPS=5,
// Burst=10) are far too conservative for a one-shot scan that may need to
// list thousands of objects. 50/100 mirrors what kube-bench and polaris
// use and stays well below the API server's per-client throttle.
const (
	defaultQPS   float32 = 50
	defaultBurst int     = 100
)

// NewClient builds a *kubernetes.Clientset using the three-tier loader
// described in the package doc. It returns both the clientset (for typed
// API calls) and the underlying *rest.Config (occasionally needed by
// callers that want to construct a dynamic client or a discovery client).
//
// Arguments:
//   - kubeconfigPath: explicit path to a kubeconfig file, or "" to use the
//     standard search path / in-cluster fallback.
//   - contextName:    if non-empty, override the current-context inside the
//     selected kubeconfig. Ignored when running in-cluster.
//
// On any failure the returned error is wrapped so callers can both display
// a clear message to the user and errors.Is() / errors.As() against the
// underlying cause if they want to programmatically branch.
func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, *rest.Config, error) {
	cfg, err := loadConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, nil, err
	}

	// Apply our sensible defaults. We do this after the loader returns so
	// the same defaults apply whether we came from kubeconfig or in-cluster.
	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.UserAgent = userAgent

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return cs, cfg, nil
}

// loadConfig walks the three tiers and returns the first rest.Config that
// succeeds, or a useful error.
func loadConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	// Tier 1: explicit path.
	if kubeconfigPath != "" {
		// Surface a clearer error than client-go does when the file is
		// missing: it returns "stat ...: no such file or directory" which
		// reads as a syscall failure rather than a configuration mistake.
		if _, statErr := os.Stat(kubeconfigPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, fmt.Errorf("no kubeconfig found at %s: file does not exist", kubeconfigPath)
			}
			return nil, fmt.Errorf("cannot read kubeconfig at %s: %w", kubeconfigPath, statErr)
		}
		cfg, err := buildFromKubeconfig(kubeconfigPath, contextName)
		if err != nil {
			return nil, fmt.Errorf("loading kubeconfig %s: %w", kubeconfigPath, err)
		}
		return cfg, nil
	}

	// Tier 2: standard search path (KUBECONFIG env, then ~/.kube/config).
	// We resolve the path ourselves so the error message can name it.
	if path := defaultKubeconfigPath(); path != "" {
		if _, statErr := os.Stat(path); statErr == nil {
			cfg, err := buildFromKubeconfig(path, contextName)
			if err != nil {
				return nil, fmt.Errorf("loading kubeconfig %s: %w", path, err)
			}
			return cfg, nil
		}
		// Fall through to the in-cluster attempt: the file the env or
		// home dir pointed at does not exist (yet).
	}

	// Tier 3: in-cluster.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf(
				"no kubeconfig found and not running in-cluster: " +
					"set --kubeconfig, set KUBECONFIG, or create ~/.kube/config",
			)
		}
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	return cfg, nil
}

// buildFromKubeconfig loads a kubeconfig file and optionally overrides the
// current-context with the caller-supplied contextName.
func buildFromKubeconfig(path, contextName string) (*rest.Config, error) {
	// ClientConfigLoadingRules with ExplicitPath gives us deterministic
	// loading from exactly one file (no KUBECONFIG-list merging surprises).
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}

	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	return clientCfg.ClientConfig()
}

// defaultKubeconfigPath returns the first plausible kubeconfig path: the
// first entry in KUBECONFIG (if set), otherwise ~/.kube/config. Returns ""
// if we cannot even determine a home dir.
//
// We deliberately do not implement KUBECONFIG-list merging here. The
// standard client-go loader does that, but for our purposes we only need a
// single path to stat() and then hand to client-go. If users have a merged
// KUBECONFIG list, they can pass --kubeconfig explicitly to disambiguate.
func defaultKubeconfigPath() string {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		// KUBECONFIG can be a colon-separated list. Use the first entry.
		for _, p := range filepath.SplitList(env) {
			if p != "" {
				return p
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// CurrentContext returns the active context name from the kubeconfig that
// would be loaded by NewClient given the same kubeconfigPath. It is a
// best-effort lookup used to populate ScanReport.Metadata.Cluster.
//
// An empty return value is acceptable: it just means "we could not tell"
// and the caller (the orchestrator) leaves Cluster blank. This is exactly
// what happens when agentmoat is running in-cluster: there is no context
// name, only a service-account token.
func CurrentContext(kubeconfigPath string) string {
	// Resolve which file to inspect using the same precedence as
	// loadConfig, minus the in-cluster tier.
	path := kubeconfigPath
	if path == "" {
		path = defaultKubeconfigPath()
	}
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}

	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	rawCfg, err := loadingRules.Load()
	if err != nil || rawCfg == nil {
		return ""
	}
	return rawCfg.CurrentContext
}
