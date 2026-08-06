// doctor_bundle.go implements contenox doctor --bundle: a redacted diagnostics
// archive plus the issue URL that asks for it.
package contenoxcli

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/spf13/cobra"
)

// doctorIssueURL is where --bundle points the operator; the bundle is the
// attachment that issue asks for.
const doctorIssueURL = "https://github.com/contenox/contenox/issues/new"

// doctorBundleLogTail caps how much of each log file is archived. Diagnostics
// come from the end of a log, and an unbounded tail turns a bug report into a
// data transfer.
const doctorBundleLogTail = 256 * 1024

// doctorBundleLogNames are the log files contenox writes (cli.go's
// telemetry.log, beam_cmd.go's beam.log). Each is looked for in the workspace
// .contenox, beside the database, and in ~/.contenox.
var doctorBundleLogNames = []string{"telemetry.log", beamLogFileName}

// redactedPlaceholder is what a removed credential is replaced by; the field
// name and punctuation around it survive so a redacted log stays greppable.
const redactedPlaceholder = "[REDACTED]"

// secretPattern is one credential shape plus the template that rewrites it.
// repl is a regexp.ReplaceAllString template: capture groups keep context, the
// secret itself never reaches the archive.
type secretPattern struct {
	re   *regexp.Regexp
	repl string
}

// secretPatterns match credential shapes in free text. Named assignments come
// first: once a value is replaced, the shape rules below cannot match it again
// and inflate the reported count.
//
// Errs toward over-redaction — a bundle is written to be handed to a stranger,
// so a false positive costs a diagnostic line and a false negative costs a key.
var secretPatterns = []secretPattern{
	// Named assignments, in log text (key=value), JSON, and URL queries.
	{regexp.MustCompile(`(?i)(api[_-]?key|apikey|access[_-]?key|secret|password|passwd|token|authorization|credential)("?\s*[:=]\s*"?)([^"\s,&}]+)`), "${1}${2}" + redactedPlaceholder},
	// URL userinfo: scheme://user:secret@host.
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://[^/\s:@]+:)([^/\s@]+)(@)`), "${1}" + redactedPlaceholder + "${3}"},
	{regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._\-]{12,})`), "${1}" + redactedPlaceholder},
	// Provider key shapes, recognizable on their own.
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`), redactedPlaceholder},
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`), redactedPlaceholder},
	{regexp.MustCompile(`AIza[A-Za-z0-9_\-]{20,}`), redactedPlaceholder},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`), redactedPlaceholder},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), redactedPlaceholder},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), redactedPlaceholder},
	{regexp.MustCompile(`ya29\.[A-Za-z0-9_\-]{10,}`), redactedPlaceholder},
}

// redactSecrets replaces credential-shaped substrings in s and reports how many
// it replaced, so the count can be printed next to the bundle rather than the
// operator having to trust it silently.
func redactSecrets(s string) (string, int) {
	count := 0
	for _, p := range secretPatterns {
		n := len(p.re.FindAllStringIndex(s, -1))
		if n == 0 {
			continue
		}
		count += n
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s, count
}

// doctorBuildInfo is the environment half of a bug report: what binary ran,
// built by which toolchain, on what platform.
func doctorBuildInfo() string {
	var b strings.Builder
	fmt.Fprintf(&b, "contenox version: %s\n", cliVersion())
	fmt.Fprintf(&b, "go version:       %s\n", runtime.Version())
	fmt.Fprintf(&b, "platform:         %s/%s\n", runtime.GOOS, runtime.GOARCH)
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprintf(&b, "module:           %s\n", info.Main.Path)
		var keys []string
		settings := map[string]string{}
		for _, s := range info.Settings {
			// Build settings only; the dependency list is public and long.
			if strings.HasPrefix(s.Key, "vcs") || strings.HasPrefix(s.Key, "GO") || s.Key == "-tags" {
				settings[s.Key] = s.Value
				keys = append(keys, s.Key)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%-18s%s\n", k+":", settings[k])
		}
	}
	return b.String()
}

// doctorBundlePath resolves where the archive is written: the --bundle-out
// value, or a timestamped name in the working directory.
func doctorBundlePath(out string, now time.Time) string {
	if strings.TrimSpace(out) != "" {
		return out
	}
	return fmt.Sprintf("contenox-doctor-%s.zip", now.UTC().Format("20060102-150405"))
}

// bundleLog is one archived log: where it came from on disk, and the member
// name it takes inside the zip.
type bundleLog struct {
	member string
	path   string
}

// doctorBundleLogPaths lists the existing log files to archive across the
// workspace .contenox, the database's directory, and ~/.contenox. The member
// name carries which of those a file came from: the three directories can hold
// same-named logs, and one archive entry per source is the whole point.
func doctorBundleLogPaths(contenoxDir, dbPath string) []bundleLog {
	type source struct{ role, dir string }
	sources := []source{{"workspace", contenoxDir}}
	if dbPath != "" {
		sources = append(sources, source{"db", filepath.Dir(dbPath)})
	}
	if home, err := globalContenoxDir(); err == nil {
		sources = append(sources, source{"global", home})
	}
	seen := map[string]bool{}
	var logs []bundleLog
	for _, src := range sources {
		if strings.TrimSpace(src.dir) == "" {
			continue
		}
		for _, name := range doctorBundleLogNames {
			abs, err := filepath.Abs(filepath.Join(src.dir, name))
			if err != nil || seen[abs] {
				continue
			}
			if st, statErr := os.Stat(abs); statErr != nil || st.IsDir() {
				continue
			}
			seen[abs] = true
			logs = append(logs, bundleLog{member: "logs/" + src.role + "/" + name, path: abs})
		}
	}
	return logs
}

// readLogTail returns the last doctorBundleLogTail bytes of path, prefixed with
// a truncation note when the file was longer.
func readLogTail(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() <= doctorBundleLogTail {
		data, readErr := io.ReadAll(f)
		return string(data), readErr
	}
	if _, err := f.Seek(st.Size()-doctorBundleLogTail, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("[truncated: last %d of %d bytes]\n", doctorBundleLogTail, st.Size()) + string(data), nil
}

// writeDoctorBundle writes the archive at path and returns how many secrets
// were redacted on the way in. Every member is passed through redactSecrets —
// including the JSON report, whose backend URLs can carry a key in the query.
func writeDoctorBundle(path string, res setupcheck.Result, contenoxDir, dbPath string) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create bundle: %w", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	total := 0
	add := func(name, content string) error {
		redacted, n := redactSecrets(content)
		total += n
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, redacted)
		return err
	}

	report, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		zw.Close()
		return total, fmt.Errorf("encode report: %w", err)
	}
	if err := add("doctor.json", string(report)+"\n"); err != nil {
		zw.Close()
		return total, err
	}
	if err := add("build.txt", doctorBuildInfo()); err != nil {
		zw.Close()
		return total, err
	}
	for _, lg := range doctorBundleLogPaths(contenoxDir, dbPath) {
		content, readErr := readLogTail(lg.path)
		if readErr != nil {
			// A log we cannot read is noted in the bundle rather than failing
			// the report it is evidence for.
			content = fmt.Sprintf("[unreadable: %v]\n", readErr)
		}
		if err := add(lg.member, content); err != nil {
			zw.Close()
			return total, err
		}
	}
	if err := zw.Close(); err != nil {
		return total, fmt.Errorf("finalize bundle: %w", err)
	}
	return total, nil
}

// doctorIssueLink builds the pre-filled issue URL: the environment facts a
// maintainer asks for first, and the bundle named as the attachment. It carries
// no log content — only the operator decides what leaves their machine.
func doctorIssueLink(res setupcheck.Result, bundlePath string) string {
	ready, reason, next := doctorVerdict(res)
	verdict := "yes"
	if !ready {
		verdict = "no — " + reason
	}
	var body strings.Builder
	body.WriteString("### What happened\n\n\n\n### Diagnostics\n\n")
	fmt.Fprintf(&body, "- contenox: %s\n", cliVersion())
	fmt.Fprintf(&body, "- platform: %s/%s (go %s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(&body, "- ready: %s\n", verdict)
	fmt.Fprintf(&body, "- provider/model: %s / %s\n", orUnset(res.DefaultProvider), orUnset(res.DefaultModel))
	fmt.Fprintf(&body, "- backends: %d registered, %d reachable\n", res.BackendCount, res.ReachableBackendCount)
	if !ready {
		fmt.Fprintf(&body, "- suggested next command: `%s`\n", next)
	}
	fmt.Fprintf(&body, "\nAttach the redacted bundle: `%s`\n", bundlePath)

	q := url.Values{}
	q.Set("title", fmt.Sprintf("doctor: %s on %s/%s", strings.TrimSpace(strings.SplitN(verdict, "—", 2)[0]), runtime.GOOS, runtime.GOARCH))
	q.Set("body", body.String())
	return doctorIssueURL + "?" + q.Encode()
}

func orUnset(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unset)"
	}
	return s
}

// writeDoctorBundleIfAsked is doctor's --bundle branch: no-op without the flag,
// otherwise it writes the archive and prints its path, the redaction count, and
// the issue link to w. A bundle that cannot be written is an error — the
// operator asked for a file to attach.
func writeDoctorBundleIfAsked(cmd *cobra.Command, w io.Writer, res setupcheck.Result, contenoxDir, dbPath string) error {
	if cmd == nil {
		return nil
	}
	wanted, _ := cmd.Flags().GetBool("bundle")
	if !wanted {
		return nil
	}
	outFlag, _ := cmd.Flags().GetString("bundle-out")
	path := doctorBundlePath(outFlag, time.Now())
	redacted, err := writeDoctorBundle(path, res, contenoxDir, dbPath)
	if err != nil {
		return err
	}
	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	fmt.Fprintf(w, "\nBundle: %s\n", abs)
	fmt.Fprintf(w, "  Contents: doctor.json, build.txt, and any telemetry.log / %s found.\n", beamLogFileName)
	fmt.Fprintf(w, "  Redacted: %d credential-shaped value(s). Review the file before sharing it.\n", redacted)
	fmt.Fprintf(w, "  Report:   %s\n", doctorIssueLink(res, abs))
	return nil
}
