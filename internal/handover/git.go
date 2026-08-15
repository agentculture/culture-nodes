package handover

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultFetchTimeout bounds one fetch. A handover ref is one commit on top
// of a checkout the remote already has, so a fetch that has not finished in
// this long is not slow, it is stuck — and the only thing a stuck fetch can
// do to a worker is hold a lease it should have released.
const DefaultFetchTimeout = 2 * time.Minute

// GitFetcher measures a handover ref by fetching it with the git binary.
//
// Remote is the CONTROL PLANE's configuration and is never taken from an
// agent's report — see this package's doc comment. It may be any url the
// operator's git can reach (an ssh or https remote, or a local path for a
// single-host deployment); this package neither parses it nor rewrites it,
// beyond refusing one that git would read as an option.
type GitFetcher struct {
	// Remote is where refs are fetched from.
	Remote string
	// GitBinary is the git executable, defaulting to "git" resolved on PATH.
	GitBinary string
	// ObjectDir, when set, is a persistent bare repository fetches
	// accumulate objects in, so repeated fetches from the same remote do not
	// re-download shared history. When empty a fresh temporary one is made
	// and removed per fetch — correct, just slower.
	ObjectDir string
	// Timeout bounds one fetch. Zero means DefaultFetchTimeout.
	Timeout time.Duration
	// MaxChangedPaths caps the reported path list; zero means
	// DefaultMaxChangedPaths. Exceeding it sets Measurement.PathsTruncated
	// rather than silently shortening the list.
	MaxChangedPaths int
	// Now is the clock, defaulting to time.Now().UTC().
	Now func() time.Time
}

// Fetch retrieves ref from the configured remote and reports the commit it
// resolves to and the paths that commit changed.
//
// Nothing about the returned Measurement is read from the caller: the ref is
// re-read from FETCH_HEAD, the commit sha is git's own resolution, and the
// path list is git's own diff. A ref the remote does not have is an error,
// which is what makes "no fetchable ref" reachable at all.
func (f *GitFetcher) Fetch(ctx context.Context, ref string) (Measurement, error) {
	if f == nil || strings.TrimSpace(f.Remote) == "" {
		return Measurement{}, fmt.Errorf("handover: this fetcher has no remote configured, so it can measure nothing")
	}
	if strings.HasPrefix(f.Remote, "-") {
		return Measurement{}, fmt.Errorf("handover: remote %q would be read by git as an option, not a repository", f.Remote)
	}
	if err := ValidateRef(ref); err != nil {
		return Measurement{}, err
	}

	dir, cleanup, err := f.objectDir()
	if err != nil {
		return Measurement{}, err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()

	// A bare repository with no working tree: nothing fetched here is ever
	// checked out, so no content from a handed-over commit is written to a
	// path in the control plane's filesystem.
	if _, err := f.git(ctx, dir, "init", "--bare", "--quiet"); err != nil {
		return Measurement{}, fmt.Errorf("handover: prepare a fetch destination: %w", err)
	}

	// `--` separates the remote and refspec from options, so neither can be
	// re-read as one however the operator wrote the remote. --no-tags keeps
	// the fetch to exactly the ref asked for.
	if _, err := f.git(ctx, dir, "fetch", "--no-tags", "--quiet", "--", f.Remote, ref); err != nil {
		return Measurement{}, fmt.Errorf("handover: fetch %s from the configured remote: %w", ref, err)
	}

	head, err := f.git(ctx, dir, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}")
	if err != nil {
		return Measurement{}, fmt.Errorf("handover: resolve the fetched ref to a commit: %w", err)
	}
	commit := strings.TrimSpace(head)
	if commit == "" {
		return Measurement{}, fmt.Errorf("handover: the fetch of %s produced no commit", ref)
	}

	// --root makes a root commit report its whole tree instead of nothing;
	// on any other commit it changes nothing, and the diff is against the
	// first parent, which is the handover commit's own base.
	raw, err := f.git(ctx, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", commit)
	if err != nil {
		return Measurement{}, fmt.Errorf("handover: read the changed paths of %s: %w", commit, err)
	}

	paths, truncated := splitPaths(raw, f.maxChangedPaths())
	return Measurement{
		Ref:            ref,
		CommitSHA:      commit,
		ChangedPaths:   paths,
		PathsTruncated: truncated,
		Source:         f.Remote,
		FetchedAt:      f.now(),
	}, nil
}

// splitPaths reads git's NUL-separated -z output. NUL separation is what makes
// a path containing a newline or a quote survive intact, which matters because
// these strings land in an immutable ledger record.
func splitPaths(raw string, max int) ([]string, bool) {
	var out []string
	for _, p := range strings.Split(raw, "\x00") {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if max > 0 && len(out) > max {
		return out[:max], true
	}
	return out, false
}

func (f *GitFetcher) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, f.binary(), args...) // #nosec G204 -- fixed binary, constant argv, one ref validated by ValidateRef
	cmd.Dir = dir
	// A control-plane fetch must fail rather than block: with prompting
	// disabled, a remote that wants credentials this process does not have
	// produces an error inside the deadline instead of a hung git waiting on
	// a terminal that is not there.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(redact(args), " "), detail)
	}
	return stdout.String(), nil
}

// redact keeps a remote url out of an error string. A remote may embed a
// token, and this error is logged.
func redact(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.Contains(a, "://") || strings.Contains(a, "@") {
			out = append(out, "<remote>")
			continue
		}
		out = append(out, a)
	}
	return out
}

func (f *GitFetcher) objectDir() (string, func(), error) {
	if f.ObjectDir != "" {
		if err := os.MkdirAll(f.ObjectDir, 0o700); err != nil {
			return "", func() {}, fmt.Errorf("handover: prepare object dir %s: %w", f.ObjectDir, err)
		}
		return f.ObjectDir, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "culture-nodes-handover-")
	if err != nil {
		return "", func() {}, fmt.Errorf("handover: create a temporary fetch destination: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (f *GitFetcher) binary() string {
	if f.GitBinary != "" {
		return f.GitBinary
	}
	return "git"
}

func (f *GitFetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return DefaultFetchTimeout
}

func (f *GitFetcher) maxChangedPaths() int {
	if f.MaxChangedPaths > 0 {
		return f.MaxChangedPaths
	}
	return DefaultMaxChangedPaths
}

func (f *GitFetcher) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

// AbsRemote is a small convenience for a local-path remote in a single-host
// deployment: git resolves a relative path against its own working directory,
// which for this package is a temporary object dir, so a relative remote would
// silently mean somewhere else. Operators configuring a path remote should run
// it through here (or write an absolute path).
func AbsRemote(remote string) (string, error) {
	if remote == "" || strings.Contains(remote, "://") || strings.Contains(remote, "@") {
		return remote, nil
	}
	abs, err := filepath.Abs(remote)
	if err != nil {
		return "", fmt.Errorf("handover: resolve remote path %q: %w", remote, err)
	}
	return abs, nil
}
