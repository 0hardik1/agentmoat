// Package audit appends one JSON line per mutating agentmoat action to a
// local file (default: ~/.agentmoat/audit.jsonl).
//
// Why a separate package and not just a log line:
//
//   - Plan.md section 12.5 requires that every mutating run leaves an audit
//     trail. The trail must outlive the process (so failed runs are still
//     visible), survive concurrent invocations (multiple apply runs against
//     different clusters from the same operator workstation), and be
//     machine-readable for downstream tools.
//
//   - A package-level Append function decouples the audit format from the
//     applier and rollback callers: they only see Entry, the package owns
//     the writer, the file path, and the byte-level layout.
//
//   - In tests we point AGENTMOAT_AUDIT_PATH at a temp file so the
//     production audit log is never touched.
//
// File format
//
//   One JSON object per line (JSONL). Each object has a `ts` (RFC3339Nano),
//   `action` ("apply" | "rollback"), `dryRun` bool, `planHash` string,
//   `workload` ({kind, namespace, name}), `status` ("applied" | "failed" |
//   ...), and optional `error` string. We add fields over time but never
//   remove them: downstream parsers should ignore unknown keys.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// EnvPathOverride is the environment variable name that tests (and power
// users) can set to redirect the audit log away from the default location.
const EnvPathOverride = "AGENTMOAT_AUDIT_PATH"

// defaultRelativePath is the path under the user's home directory where the
// audit log is written when EnvPathOverride is not set. Matches plan.md
// section 12.5 ("~/.agentmoat/audit.jsonl").
const defaultRelativePath = ".agentmoat/audit.jsonl"

// fileMu serialises writes to the audit file inside this process. The file
// itself is opened with O_APPEND so the kernel guarantees atomic line writes
// across processes; the mutex is just here to avoid interleaved JSON when
// two goroutines call Append concurrently from the same binary.
var fileMu sync.Mutex

// Entry is the in-memory shape of one audit line. Callers fill in the
// fields they know; the package populates Timestamp if zero.
type Entry struct {
	// Timestamp is the moment the action occurred. RFC3339Nano on disk.
	// Append() fills this in when the value is the zero time.
	Timestamp time.Time `json:"ts"`

	// Action is "apply" or "rollback". Free-form string so future
	// mutating commands (e.g. "patch-namespace") can extend it without a
	// schema bump.
	Action string `json:"action"`

	// DryRun is true when no real mutation occurred. Recorded so the trail
	// distinguishes rehearsals from production changes.
	DryRun bool `json:"dryRun"`

	// PlanHash is the deterministic hash of the MigrationPlan that drove
	// the action. Useful for joining the audit file to the on-cluster
	// `agentmoat.io/plan-hash` annotation.
	PlanHash string `json:"planHash,omitempty"`

	// Workload identifies the object that was (or would be) mutated. Empty
	// for namespace-scoped actions like "annotate-namespace".
	Workload schema.WorkloadRef `json:"workload,omitempty"`

	// Status is the per-step outcome: "applied", "already-applied",
	// "skipped", "failed". Mirror of schema.StepStatus values.
	Status schema.StepStatus `json:"status"`

	// Error is the rendered error message when Status == "failed". Empty
	// otherwise.
	Error string `json:"error,omitempty"`
}

// Append writes one entry to the audit log. Returns the path it wrote to
// (useful in tests). A missing parent directory is created with 0700; the
// log file is opened with 0600 because the entries name cluster contexts
// and workload identities that an operator may consider sensitive.
//
// Errors from Append are returned, not logged: the calling applier surfaces
// them so the operator knows the trail may be incomplete. We do NOT abort
// the apply when audit logging fails: a missing audit line is recoverable,
// an aborted apply mid-flight is not.
func Append(entry Entry) (path string, err error) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	path, err = ResolvePath()
	if err != nil {
		return "", fmt.Errorf("resolving audit path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("creating audit dir: %w", err)
	}

	// Marshal first, so a malformed Entry doesn't leave a half-written
	// line in the file.
	line, err := json.Marshal(entry)
	if err != nil {
		return path, fmt.Errorf("marshalling audit entry: %w", err)
	}
	line = append(line, '\n')

	fileMu.Lock()
	defer fileMu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return path, fmt.Errorf("opening audit file %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return path, fmt.Errorf("writing audit line: %w", err)
	}
	return path, nil
}

// ResolvePath returns the absolute path the audit log will be written to.
// Honors AGENTMOAT_AUDIT_PATH; falls back to ~/.agentmoat/audit.jsonl.
// Exported so the CLI can print "wrote audit line to <path>" on success.
func ResolvePath() (string, error) {
	if env := os.Getenv(EnvPathOverride); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir: %w", err)
	}
	return filepath.Join(home, defaultRelativePath), nil
}
