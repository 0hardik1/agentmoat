// Package seccompprobe: the reader pod's script and the parser for its
// output.
package seccompprobe

import (
	"bufio"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// readerScript runs in the reader pod (busybox sh and awk). It walks the
// Sentry boot logs in the mounted directory, keeps the ones that belong to
// this run (a trial pod's name ends in "-<run id>", and the boot log holds
// each container's OCI spec with the CRI sandbox-name annotation), and
// prints one compact block per file:
//
//	F <file>
//	V <runsc release> <GOARCH>       from "cli.go] Version release-..., go..., amd64, ..."
//	N <pod name>                      io.kubernetes.cri.sandbox-name
//	S <pod namespace>                 io.kubernetes.cri.sandbox-namespace
//	I <n>                             "Installing OCI seccomp filter(s)" lines
//	G <n>                             "Seccomp spec is being ignored because oci-seccomp is disabled" lines
//	D <syscall number> <count>        "Syscall <n>: denied by seccomp"
//	K <syscall number> <count>        "Syscall <n>: killed by seccomp[ (process)]"
//	E
//
// and "DONE <files matched>" at the end, so a truncated log is detectable.
// The log lines it matches are verbatim from gVisor release-20261123.1:
// runsc/cli/cli.go, runsc/boot/seccomp.go, pkg/sentry/kernel/task_syscall.go
// and specutils.LogSpecDebug. The script prints counts, not log lines, so
// nothing else from the node (paths, environment, other pods' specs)
// leaves it.
const readerScript = `set -u
cd "$LOG_DIR" || { echo "ERR cannot enter $LOG_DIR"; exit 3; }
found=0
for f in *.boot.txt; do
  [ -f "$f" ] || continue
  grep -q -- "-$RUN_ID\"" "$f" || continue
  found=$((found+1))
  echo "F $f"
  awk '
    /\] Version release-/ && !v { s=$0; sub(/.*\] Version /, "", s); n=split(s, a, ", "); print "V " a[1] " " a[3]; v=1 }
    /"io.kubernetes.cri.sandbox-name": "/ && !pn { s=$0; sub(/.*"io.kubernetes.cri.sandbox-name": "/, "", s); sub(/".*/, "", s); print "N " s; pn=1 }
    /"io.kubernetes.cri.sandbox-namespace": "/ && !ps { s=$0; sub(/.*"io.kubernetes.cri.sandbox-namespace": "/, "", s); sub(/".*/, "", s); print "S " s; ps=1 }
    /Installing OCI seccomp filter/ { inst++ }
    /Seccomp spec is being ignored because oci-seccomp is disabled/ { ign++ }
    /Syscall [0-9]+: denied by seccomp/ { s=$0; sub(/.*Syscall /, "", s); sub(/:.*/, "", s); d[s]++ }
    /Syscall [0-9]+: killed by seccomp/ { s=$0; sub(/.*Syscall /, "", s); sub(/:.*/, "", s); k[s]++ }
    END { print "I " inst+0; print "G " ign+0; for (x in d) print "D " x " " d[x]; for (x in k) print "K " x " " k[x]; print "E" }
  ' "$f"
done
echo "DONE $found"
`

// sandboxLog is what the reader saw in one Sentry boot log.
type sandboxLog struct {
	File      string
	Version   string
	Arch      string
	Pod       string
	Namespace string
	Installed int
	Ignored   int
	Denied    map[int]int
	Killed    map[int]int
}

// parseReaderOutput parses readerScript's output. It fails when the
// output does not end with the DONE line (the log was cut off, or the
// script never ran) or a line does not parse.
func parseReaderOutput(out string) ([]sandboxLog, error) {
	var logs []sandboxLog
	var cur *sandboxLog
	done := false
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		tag, rest, _ := strings.Cut(line, " ")
		if tag == "F" {
			logs = append(logs, sandboxLog{File: rest, Denied: map[int]int{}, Killed: map[int]int{}})
			cur = &logs[len(logs)-1]
			continue
		}
		switch tag {
		case "DONE":
			done = true
			continue
		case "ERR":
			return nil, fmt.Errorf("reader: %s", rest)
		}
		if cur == nil {
			return nil, fmt.Errorf("unexpected line before any file: %q", line)
		}
		if err := cur.apply(tag, rest); err != nil {
			return nil, fmt.Errorf("file %s: %w", cur.File, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !done {
		return nil, fmt.Errorf("output ends without the DONE line (cut off?)")
	}
	return logs, nil
}

func (l *sandboxLog) apply(tag, rest string) error {
	var err error
	switch tag {
	case "V":
		l.Version, l.Arch, _ = strings.Cut(rest, " ")
	case "N":
		l.Pod = rest
	case "S":
		l.Namespace = rest
	case "I":
		l.Installed, err = strconv.Atoi(rest)
	case "G":
		l.Ignored, err = strconv.Atoi(rest)
	case "D", "K":
		nr, count, ok := strings.Cut(rest, " ")
		if !ok {
			return fmt.Errorf("bad %s line %q", tag, rest)
		}
		n, e1 := strconv.Atoi(nr)
		c, e2 := strconv.Atoi(count)
		if e1 != nil || e2 != nil {
			return fmt.Errorf("bad %s line %q", tag, rest)
		}
		if tag == "D" {
			l.Denied[n] += c
		} else {
			l.Killed[n] += c
		}
	case "E":
	default:
		return fmt.Errorf("unknown line %q", tag+" "+rest)
	}
	return err
}

// merge folds the logs of one trial pod into its result. A pod can have
// more than one boot log when its sandbox was recreated; the counts add up.
func merge(w *schema.SeccompWorkload, logs []sandboxLog) {
	denied, killed := map[int]int{}, map[int]int{}
	arch := ""
	for _, l := range logs {
		if w.RunscVersion == "" {
			w.RunscVersion = l.Version
		}
		if arch == "" {
			arch = l.Arch
		}
		w.FiltersLoaded += l.Installed
		w.FiltersIgnored += l.Ignored
		for n, c := range l.Denied {
			denied[n] += c
		}
		for n, c := range l.Killed {
			killed[n] += c
		}
	}
	w.Blocked = nil
	for n, c := range denied {
		w.Blocked = append(w.Blocked, schema.BlockedSyscall{Name: SyscallName(arch, n), Number: n, Action: "denied", Count: c})
	}
	for n, c := range killed {
		w.Blocked = append(w.Blocked, schema.BlockedSyscall{Name: SyscallName(arch, n), Number: n, Action: "killed", Count: c})
	}
	sort.Slice(w.Blocked, func(i, j int) bool {
		a, b := w.Blocked[i], w.Blocked[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Action < b.Action
	})
}

// SyscallName returns the name of syscall nr on arch ("amd64", "arm64"),
// or "syscall-<nr>" when the table has no name for it. An unknown arch is
// read as amd64, the only other one agentmoat's node images ship.
func SyscallName(arch string, nr int) string {
	table := syscallNamesAmd64
	if arch == "arm64" {
		table = syscallNamesArm64
	}
	if name, ok := table[nr]; ok {
		return name
	}
	return fmt.Sprintf("syscall-%d", nr)
}
