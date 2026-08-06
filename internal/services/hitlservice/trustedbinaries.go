package hitlservice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Identity and integrity for the commands an allow rule names. A prefix
// allowlist pins a NAME; PATH decides what that name is, so any writable
// directory earlier in PATH aliasing `go`, `npm` or `git` converts an allow
// rule into blessed arbitrary execution. TrustedBinaries closes that by
// resolving the name to an absolute real path at evaluation time and checking
// it against what the operator declared.
//
// Monotonic by construction: it is consulted ONLY on the allow path
// (commandPrefixAllowMatch) and can only withdraw an allow, which then falls
// through to a later rule and ultimately the approve floor. It never upgrades
// an ask and never relaxes a deny.
//
// Scope bar (the properly-set-up-system assumption): this defends against a
// swapped or tampered binary, and against a name that resolves somewhere the
// operator never declared. It does not defend against a compromised kernel, a
// hostile operator running as root, or anyone who can already write to the
// declared directories. See docs/guide/trusted-binaries.md.

// TrustedBinaries is the envelope's identity+integrity block, declared once
// per policy under "trusted_binaries". Both halves are opt-in and independent:
//
//   - Dirs alone answers "where may a name come from" (H2, identity).
//   - Hashes alone answers "is this the binary I declared" (H3, integrity),
//     and is a STRICT PIN: once any hash is declared, a command with no
//     declared hash is refused rather than waved through.
//
// An absent (or wholly empty) block changes nothing — today's verdicts stand.
type TrustedBinaries struct {
	// Dirs are absolute directories a resolved binary may live under, at any
	// depth. Symlinks are resolved on both sides before the comparison.
	Dirs []string `json:"dirs,omitempty"`
	// Hashes maps an absolute REAL path (post-symlink-resolution) to its
	// lowercase or uppercase hex SHA256. The trust verb writes real paths;
	// declaring a symlink here never matches, and the refusal names the real
	// path so the correction is obvious.
	Hashes map[string]string `json:"hashes,omitempty"`
}

// Defensive caps on the declaration block: they reject a runaway or a
// generated-file accident, never a legitimately large fleet.
const (
	maxTrustedDirs        = 256
	maxTrustedHashes      = 8192
	maxTrustedEntryBytes  = 4096
	sha256HexLen          = 64
	maxHashCacheEntries   = 1024
	trustedBinaryHashAlgo = "sha256"
)

// The refusal texts. Every one of them ends in a refusal, never a warning:
// there is no warn-and-run mode. The hash-mismatch wording is fixed by the
// hardening plan and is quoted verbatim in docs/guide/trusted-binaries.md.
const (
	trustRefusalUnresolved  = "command %q does not resolve to an executable on this host — allow refused"
	trustRefusalRelative    = "command %q is a relative path the policy cannot resolve — allow refused; name the command or give it an absolute path"
	trustRefusalOutsideDirs = "binary at %s is not under any trusted_binaries.dirs entry — allow refused; declare its directory after verifying what it is"
	trustRefusalUndeclared  = "binary at %s has no declared hash — allow refused; declare it with `contenox hitl trust %s`"
	trustRefusalUnreadable  = "binary at %s could not be read for hashing (%v) — allow refused"
	trustRefusalMismatch    = "binary at %s does not match the declared hash — re-declare after verifying the upgrade, or investigate"
)

// errRelativeCommand marks a command word that names a path relative to a
// working directory the policy does not know; resolving it would be a guess.
var errRelativeCommand = errors.New("relative command path")

// enforced reports whether the block gates anything. A nil or empty block is
// inert, which is what every policy written before this existed gets.
func (tb *TrustedBinaries) enforced() bool {
	return tb != nil && (len(tb.Dirs) > 0 || len(tb.Hashes) > 0)
}

// verifyCommand resolves one command word the way the host will and checks it
// against the declarations. It returns "" when the binary is trusted, else the
// operator-facing refusal that revoked the allow.
func (tb *TrustedBinaries) verifyCommand(name string) string {
	real, err := resolveBinary(name)
	if err != nil {
		if errors.Is(err, errRelativeCommand) {
			return fmt.Sprintf(trustRefusalRelative, name)
		}
		return fmt.Sprintf(trustRefusalUnresolved, name)
	}
	if len(tb.Dirs) > 0 && !underAnyTrustedDir(real, tb.Dirs) {
		return fmt.Sprintf(trustRefusalOutsideDirs, real)
	}
	if len(tb.Hashes) == 0 {
		return ""
	}
	// STRICT PIN: declared or refuse. A "record on first use" mode would
	// weaken exactly what this exists for.
	want, ok := lookupDeclaredHash(tb.Hashes, real)
	if !ok {
		return fmt.Sprintf(trustRefusalUndeclared, real, real)
	}
	got, err := binarySHA256(real)
	if err != nil {
		return fmt.Sprintf(trustRefusalUnreadable, real, err)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Sprintf(trustRefusalMismatch, real)
	}
	return ""
}

// resolveBinary answers "which file will this command word actually run",
// then resolves symlinks so the answer is the real file and not an alias of
// it. Bare names go through exec.LookPath — the same resolution the executing
// process performs, and on Windows the same PATHEXT search `where.exe` does —
// which is why the ANSWER is then checked against a declared set rather than
// trusted for being a lookup.
func resolveBinary(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("empty command")
	}
	var candidate string
	switch {
	case filepath.IsAbs(name):
		candidate = name
	case commandHasPathSeparator(name):
		return "", errRelativeCommand
	default:
		p, err := exec.LookPath(name)
		if err != nil {
			return "", err
		}
		candidate = p
	}
	// EvalSymlinks is load-bearing twice: it defeats an alias pointing at
	// another binary, and on Windows it canonicalizes the CASE that LookPath
	// returns lowercased from PATHEXT (observed: LookPath "foo" ->
	// ...\foo.cmd, EvalSymlinks -> ...\foo.CMD).
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// commandHasPathSeparator mirrors exec.LookPath's own "this is a path, not a
// name" test, per platform: a backslash is an ordinary filename byte on POSIX.
func commandHasPathSeparator(name string) bool {
	if strings.ContainsRune(name, '/') {
		return true
	}
	return runtime.GOOS == "windows" && strings.ContainsRune(name, '\\')
}

// pathKey normalizes a path for comparison: case-folded on Windows, where the
// filesystem is, and byte-exact everywhere else.
func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// underAnyTrustedDir reports whether real lives under a declared directory at
// any depth. Declared directories are symlink-resolved too, so /var vs
// /private/var (macOS) and a symlinked /usr/local are not spurious refusals.
func underAnyTrustedDir(real string, dirs []string) bool {
	target := pathKey(real)
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		prefix := pathKey(dir)
		if prefix == "" {
			continue
		}
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

// lookupDeclaredHash finds real's declared hash, comparing paths the way the
// platform does.
func lookupDeclaredHash(hashes map[string]string, real string) (string, bool) {
	if h, ok := hashes[real]; ok {
		return strings.TrimSpace(h), true
	}
	key := pathKey(real)
	for declared, h := range hashes {
		if pathKey(declared) == key {
			return strings.TrimSpace(h), true
		}
	}
	return "", false
}

// hashCacheEntry is one cached digest plus the change signal it was computed
// under.
type hashCacheEntry struct {
	size    int64
	modTime time.Time
	sum     string
}

var (
	hashCacheMu sync.Mutex
	hashCache   = map[string]hashCacheEntry{}
)

// binarySHA256 returns path's hex SHA256, hashing it only when (size, mtime)
// changed since the last call — hashing a 100MB toolchain binary on every
// tool call is not affordable, and inode is deliberately not part of the key
// because Windows has none.
//
// The cache's trust assumption, stated plainly: an attacker who can rewrite
// the binary can also restore its size and mtime (`touch -r`), and this cache
// would then serve the stale digest for the life of the process. That is
// inside the properly-set-up-system assumption — someone with write access to
// a declared trusted directory is already past this control. The mitigation
// is the same one the whole feature rests on: keep trusted directories
// unwritable by the agent and by anything it runs.
func binarySHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := pathKey(path)
	hashCacheMu.Lock()
	entry, ok := hashCache[key]
	hashCacheMu.Unlock()
	if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.sum, nil
	}
	sum, err := hashFileSHA256(path)
	if err != nil {
		return "", err
	}
	hashCacheMu.Lock()
	// Bounded by wholesale reset rather than an eviction policy: the cache is
	// a cost optimization, and a cold cache only costs one re-read.
	if len(hashCache) >= maxHashCacheEntries {
		hashCache = map[string]hashCacheEntry{}
	}
	hashCache[key] = hashCacheEntry{size: info.Size(), modTime: info.ModTime(), sum: sum}
	hashCacheMu.Unlock()
	return sum, nil
}

// hashFileSHA256 reads path in full and returns its lowercase hex SHA256 —
// the same digest `sha256sum`, `shasum -a 256`, and
// `(Get-FileHash -Algorithm SHA256 <path>).Hash` produce (the last in
// uppercase; comparisons here are case-insensitive).
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ResolveTrustedBinary is the trust verb's half of the same resolution the
// evaluator performs: it returns the absolute real path a command word runs
// and that file's hex SHA256, so a declaration written by `contenox hitl
// trust` is by construction the one the evaluator will look up.
func ResolveTrustedBinary(name string) (realPath, sha256Hex string, err error) {
	real, err := resolveBinary(name)
	if err != nil {
		return "", "", err
	}
	sum, err := hashFileSHA256(real)
	if err != nil {
		return "", "", err
	}
	return real, sum, nil
}

// validateTrustedBinaries checks the block's shape — absolute paths and
// well-formed digests — never its tightness. A malformed block fails the
// policy to load, which falls back to the rule-free approve-everything
// default rather than silently disarming the pin.
func validateTrustedBinaries(tb *TrustedBinaries) error {
	if tb == nil {
		return nil
	}
	if len(tb.Dirs) > maxTrustedDirs {
		return fmt.Errorf("trusted_binaries: too many dirs (%d, max %d)", len(tb.Dirs), maxTrustedDirs)
	}
	for i, dir := range tb.Dirs {
		trimmed := strings.TrimSpace(dir)
		if trimmed == "" {
			return fmt.Errorf("trusted_binaries: dirs entry %d is empty", i)
		}
		if len(trimmed) > maxTrustedEntryBytes {
			return fmt.Errorf("trusted_binaries: dirs entry %d exceeds max length (%d bytes, max %d)", i, len(trimmed), maxTrustedEntryBytes)
		}
		if !filepath.IsAbs(trimmed) {
			return fmt.Errorf("trusted_binaries: dirs entry %d %q must be an absolute path — a relative directory has no fixed meaning at evaluation time", i, trimmed)
		}
	}
	if len(tb.Hashes) > maxTrustedHashes {
		return fmt.Errorf("trusted_binaries: too many hashes (%d, max %d)", len(tb.Hashes), maxTrustedHashes)
	}
	for _, path := range sortedKeys(tb.Hashes) {
		if strings.TrimSpace(path) == "" {
			return errors.New("trusted_binaries: hashes has an empty path key")
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("trusted_binaries: hashes key %q must be an absolute path — declare the real path the command resolves to", path)
		}
		if err := validateSHA256Hex(path, tb.Hashes[path]); err != nil {
			return err
		}
	}
	return nil
}

func validateSHA256Hex(path, value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("trusted_binaries: hashes[%q] is empty — declare a %s digest or remove the entry", path, trustedBinaryHashAlgo)
	}
	if len(v) != sha256HexLen {
		return fmt.Errorf("trusted_binaries: hashes[%q] is not a %s digest (%d hex characters, expected %d)", path, trustedBinaryHashAlgo, len(v), sha256HexLen)
	}
	if _, err := hex.DecodeString(v); err != nil {
		return fmt.Errorf("trusted_binaries: hashes[%q] is not hexadecimal: %w", path, err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TrustedBinaryStatus is one declared entry checked against the host, for
// `contenox vet` and `contenox doctor`. State is one of the trustedBinary*
// constants below.
type TrustedBinaryStatus struct {
	Path  string `json:"path"`
	State string `json:"state"`
	// Actual is the digest found on disk when State is TrustedBinaryMismatch.
	Actual string `json:"actual,omitempty"`
	// Detail carries the read error when State is TrustedBinaryUnreadable.
	Detail string `json:"detail,omitempty"`
}

// The states a declared entry can be in on this host.
const (
	// TrustedBinaryOK: the file exists and its digest matches.
	TrustedBinaryOK = "ok"
	// TrustedBinaryMissing: nothing is at the declared path.
	TrustedBinaryMissing = "missing"
	// TrustedBinaryMismatch: the file exists and hashes to something else.
	TrustedBinaryMismatch = "mismatch"
	// TrustedBinaryUnreadable: the file exists but could not be read.
	TrustedBinaryUnreadable = "unreadable"
	// TrustedBinaryOutsideDirs: the entry is declared outside every dir, so
	// the identity check refuses it before its hash is ever consulted.
	TrustedBinaryOutsideDirs = "outside_dirs"
)

// CheckTrustedBinaries reports every declared entry that is not simply OK on
// this host — the material `vet` and `doctor` show an operator. It reads and
// hashes files, so it belongs to host-aware callers, not to the pure
// document validators. Returns nil when everything checks out.
func CheckTrustedBinaries(tb *TrustedBinaries) []TrustedBinaryStatus {
	if tb == nil || len(tb.Hashes) == 0 {
		return nil
	}
	var out []TrustedBinaryStatus
	for _, path := range sortedKeys(tb.Hashes) {
		want := strings.TrimSpace(tb.Hashes[path])
		if _, err := os.Stat(path); err != nil {
			out = append(out, TrustedBinaryStatus{Path: path, State: TrustedBinaryMissing, Detail: err.Error()})
			continue
		}
		if len(tb.Dirs) > 0 && !underAnyTrustedDir(path, tb.Dirs) {
			out = append(out, TrustedBinaryStatus{Path: path, State: TrustedBinaryOutsideDirs})
			continue
		}
		got, err := hashFileSHA256(path)
		if err != nil {
			out = append(out, TrustedBinaryStatus{Path: path, State: TrustedBinaryUnreadable, Detail: err.Error()})
			continue
		}
		if !strings.EqualFold(got, want) {
			out = append(out, TrustedBinaryStatus{Path: path, State: TrustedBinaryMismatch, Actual: got})
		}
	}
	return out
}

// String renders one status as the single line vet and doctor both print.
func (s TrustedBinaryStatus) String() string {
	switch s.State {
	case TrustedBinaryMissing:
		return fmt.Sprintf("%s: declared but not present on this host — remove the entry or restore the binary", s.Path)
	case TrustedBinaryMismatch:
		return fmt.Sprintf("%s: does not match the declared hash (on disk %s) — re-declare after verifying the upgrade, or investigate", s.Path, s.Actual)
	case TrustedBinaryUnreadable:
		return fmt.Sprintf("%s: declared but unreadable (%s) — calls naming it will be refused", s.Path, s.Detail)
	case TrustedBinaryOutsideDirs:
		return fmt.Sprintf("%s: declared outside every trusted_binaries.dirs entry — its hash can never be reached", s.Path)
	default:
		return fmt.Sprintf("%s: %s", s.Path, s.State)
	}
}
