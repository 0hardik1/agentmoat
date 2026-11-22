// Package agentmoat: the classifier rule registry, built one way.
//
// Scan, AssessWorkload, and the deep Explain all classify workloads, so all
// three need the same registry: the built-in rules plus the optional YAML
// override file (--rules). Each of them used to carry its own copy of this
// block. One function keeps them from drifting: a verdict from `scan` and a
// verdict from `explain workload` for the same object must come from the
// same rules.
package agentmoat

import (
	"fmt"
	"io"
	"os"

	"github.com/0hardik1/agentmoat/pkg/classifier"
)

// buildRegistry returns a registry with the built-in rules and, when
// rulesPath is not empty, the severity overrides from that YAML file.
//
// A file that cannot be read or parsed is an error. An override for a rule
// ID that does not exist is only a warning on stderr: the operator may have
// written the file for a newer agentmoat.
func buildRegistry(rulesPath string, stderr io.Writer) (*classifier.Registry, error) {
	registry := classifier.NewRegistry()
	classifier.RegisterBuiltins(registry)
	if rulesPath == "" {
		return registry, nil
	}

	// #nosec G304 -- rulesPath is the operator's --rules flag; reading the
	// file they point at is the feature.
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		return nil, fmt.Errorf("reading rules override %s: %w", rulesPath, err)
	}
	warns, err := registry.LoadYAML(data)
	if err != nil {
		return nil, fmt.Errorf("parsing rules override %s: %w", rulesPath, err)
	}
	for _, w := range warns {
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return registry, nil
}
