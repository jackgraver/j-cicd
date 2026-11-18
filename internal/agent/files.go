package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A files project is deployed from artifacts (binaries, mostly) instead of
// images, with the same guarantees as a compose deploy:
//
//   - the release is named by content: each file by its sha256, the way
//     an image is by its digest, and every stored copy is checked against
//     it before it's installed;
//   - nothing changes until everything that can be checked up front has
//     been (files received and verified, target directories writable,
//     the unit known to systemd, pre_deploy passed);
//   - each file is replaced atomically (written beside it, renamed over
//     it), so a running process keeps its old inode and nothing ever
//     sees half a binary;
//   - the unit is restarted and must stay up, then check: must pass, or
//     the previous files go back and the unit is restarted on them;
//   - every installed release is kept in <state_dir>/<project>.files/, so
//     rollback needs nothing from the build host.

// untrackedTag labels a file the agent found on disk rather than
// installed: the release that was there before the first deploy, or one
// replaced by hand since.
const untrackedTag = "untracked"

// maxArtifact bounds one received file.
const maxArtifact = 2 << 30

var fileRef = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,127})@sha256:([a-f0-9]{64})$`)

// fileRelease is one file's part of a files deploy.
type fileRelease struct {
	file *File
	hash string // hex sha256 of the contents
	rev  string // the revision it was built from, "" if unknown
}

func (r fileRelease) ref() string { return r.file.Name + "@sha256:" + r.hash }

func (r fileRelease) label() string {
	if r.rev == "" {
		return "sha256:" + shortID(r.hash)
	}
	return r.rev + " (sha256:" + shortID(r.hash) + ")"
}

// resolveFiles maps each name@sha256:<hex> ref to the file it replaces.
// As with images, a name the project doesn't have is an error: it means
// .cicd.yaml and this config disagree.
func (p *Project) resolveFiles(refs []string, revision string) ([]fileRelease, error) {
	if len(refs) == 0 {
		return nil, errors.New("no artifacts to deploy")
	}
	seen := map[string]bool{}
	var out []fileRelease
	for _, ref := range refs {
		m := fileRef.FindStringSubmatch(ref)
		if m == nil {
			return nil, fmt.Errorf("%q is not name@sha256:<hex>", ref)
		}
		f := p.fileNamed(m[1])
		if f == nil {
			return nil, fmt.Errorf("project %q has no file %q (check files: in the agent config against artifacts: in .cicd.yaml)", p.Name, m[1])
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("file %q was given twice", f.Name)
		}
		seen[f.Name] = true
		out = append(out, fileRelease{file: f, hash: m[2], rev: revision})
	}
	return out, nil
}

// DeployFiles installs refs (name@sha256:<hex>) for a files project:
// receive them from artifacts (a tar stream, as j-cicd run sends; nil to
// deploy files already in the store), check everything that can be
// checked, replace the files, restart the unit, wait for it to stay up,
// run check:, and put the previous files back if any of that fails.
// revision is what the files were built from, for the record and check.
func (a *Agent) DeployFiles(ctx context.Context, projectName string, refs []string, revision string, artifacts io.Reader) error {
	proj, err := a.cfg.Project(projectName)
	if err != nil {
		return err
	}
	if !proj.IsFiles() {
		return fmt.Errorf("project %q runs compose services, so it is deployed with images, not artifacts", proj.Name)
	}
	if revision != "" && !tagName.MatchString(revision) {
		return fmt.Errorf("revision %q is not a valid revision", revision)
	}
	rels, err := proj.resolveFiles(refs, revision)
	if err != nil {
		return err
	}
	return a.locked(ctx, proj, func(st *State) error {
		// Under the lock: pruning after another deploy can't then delete
		// what this one just received.
		if artifacts != nil {
			if err := a.receive(proj, rels, artifacts); err != nil {
				a.recordFilesUntouched(proj, st, rels, resultReceiveFailed)
				return fmt.Errorf("receiving artifacts: %w (nothing was changed)", err)
			}
		}
		return a.deployFiles(ctx, proj, st, rels)
	})
}

// rollbackFiles reverts the most recent successful deploy of a files
// project: each file it changed goes back to the one it replaced, through
// the same checks, restart and automatic rollback as any deploy.
func (a *Agent) rollbackFiles(ctx context.Context, proj *Project) error {
	return a.locked(ctx, proj, func(st *State) error {
		rels, err := lastDeployedFiles(proj, st)
		if err != nil {
			return err
		}
		a.log.Printf("rolling back %q's last deploy to %s", proj.Name, describeFiles(rels))
		return a.deployFiles(ctx, proj, st, rels)
	})
}

// lastDeployedFiles returns, for each file changed by the most recent
// successful deploy, the release it replaced.
func lastDeployedFiles(proj *Project, st *State) ([]fileRelease, error) {
	var latest time.Time
	for i := range proj.Files {
		if ss := st.Services[proj.Files[i].Name]; ss != nil && ss.Current != "" && ss.since().After(latest) {
			latest = ss.since()
		}
	}
	if latest.IsZero() {
		return nil, fmt.Errorf("%q has never been deployed: nothing to roll back", proj.Name)
	}
	var out []fileRelease
	var stranded []string
	for i := range proj.Files {
		f := &proj.Files[i]
		ss := st.Services[f.Name]
		if ss == nil || ss.Current == "" || !ss.since().Equal(latest) {
			continue
		}
		if ss.Previous == "" {
			stranded = append(stranded, f.Name)
			continue
		}
		out = append(out, fileRelease{file: f, hash: hexOf(ss.Previous), rev: ss.PreviousTag})
	}
	if len(stranded) > 0 {
		return nil, fmt.Errorf("no previous release of %v recorded (nothing was installed there before): nothing to roll back to", stranded)
	}
	return out, nil
}

// deployFiles is the body of a files deploy or manual rollback, with the
// lock held and state loaded.
func (a *Agent) deployFiles(ctx context.Context, proj *Project, st *State, rels []fileRelease) error {
	dctx, cancel := context.WithTimeout(ctx, proj.timeout())
	defer cancel()

	if err := a.preflightFiles(dctx, proj, rels); err != nil {
		a.recordFilesUntouched(proj, st, rels, resultPreflightFailed)
		return fmt.Errorf("%w (nothing was changed)", err)
	}

	var changed []fileRelease
	for _, r := range rels {
		if err := a.reconcile(proj, st, r); err != nil {
			a.recordFilesUntouched(proj, st, rels, resultPreflightFailed)
			return fmt.Errorf("%w (nothing was changed)", err)
		}
		ss := st.file(r.file)
		if hexOf(ss.Current) == r.hash {
			a.log.Printf("[%s] already installed: %s", r.file.Name, r.label())
			continue
		}
		if ss.Current == "" {
			a.log.Printf("[%s] warning: nothing is installed at %s yet, so this deploy cannot be rolled back", r.file.Name, r.file.Path)
		}
		changed = append(changed, r)
	}
	if len(changed) == 0 {
		a.log.Printf("nothing to deploy for %q", proj.Name)
		return st.save(a.cfg.StateDir, proj.Name)
	}
	if proj.Systemd == nil && len(proj.Check) == 0 {
		a.log.Printf("warning: %q has no systemd: unit and no check:, so nothing confirms the new files work", proj.Name)
	}

	names, parts := make([]string, len(changed)), make([]string, len(changed))
	for i, r := range changed {
		names[i], parts[i] = r.file.Name, r.file.Name+"="+shortFileLabel(r)
	}
	if err := a.runPreDeploy(dctx, proj, "", names, parts); err != nil {
		a.recordFilesUntouched(proj, st, changed, resultPreDeployFailed)
		return fmt.Errorf("pre_deploy failed: %w (nothing was changed)", err)
	}

	a.log.Printf("deploying %s", describeFiles(changed))
	deployErr := a.installAll(proj, st, changed)
	if deployErr == nil {
		deployErr = a.activate(dctx, proj, changed[0].rev)
	} else if errors.Is(deployErr, errRestored) {
		a.recordFilesUntouched(proj, st, changed, resultInstallFailed)
		return deployErr
	}

	now := a.now().UTC()
	for _, r := range changed {
		ss := st.file(r.file)
		ss.LastAttempt, ss.LastDeploy = r.ref(), now
	}
	if deployErr == nil {
		for _, r := range changed {
			ss := st.file(r.file)
			ss.Previous, ss.PreviousTag = ss.Current, ss.CurrentTag
			ss.Current, ss.CurrentTag, ss.CurrentSince, ss.LastResult = "sha256:"+r.hash, r.rev, now, resultHealthy
		}
		if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
			return fmt.Errorf("deploy succeeded but recording it failed: %w", err)
		}
		if err := st.pruneBlobs(a.cfg.StateDir, proj); err != nil {
			a.log.Printf("warning: removing old releases: %v", err)
		}
		a.log.Printf("deployed %q: %s", proj.Name, describeFiles(changed))
		return nil
	}

	a.log.Printf("deploy of %q failed: %v", proj.Name, deployErr)
	// As with compose: roll back even if cancelled, with a fresh budget.
	rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), proj.timeout())
	defer rbCancel()
	rbErr := a.autoRollbackFiles(rbCtx, proj, st, changed)
	if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
		a.log.Printf("warning: recording the failed deploy: %v", err)
	}
	if rbErr != nil {
		return fmt.Errorf("%w; ROLLBACK FAILED, needs attention: %v", deployErr, rbErr)
	}
	return fmt.Errorf("%w (rolled back to the previous release)", deployErr)
}

// autoRollbackFiles puts back the last healthy release of each changed
// file, restarts the unit on it and checks it. Exactly one step back,
// never a retry of the failed release.
func (a *Agent) autoRollbackFiles(ctx context.Context, proj *Project, st *State, changed []fileRelease) error {
	var back []fileRelease
	var stranded []string
	for _, r := range changed {
		ss := st.file(r.file)
		if ss.Current == "" {
			ss.LastResult = resultNoRollback
			stranded = append(stranded, r.file.Name)
			continue
		}
		back = append(back, fileRelease{file: r.file, hash: hexOf(ss.Current), rev: ss.CurrentTag})
	}
	var err error
	if len(back) > 0 {
		a.log.Printf("rolling back to %s", describeFiles(back))
		err = a.installAll(proj, st, back)
		if err == nil || errors.Is(err, errRestored) {
			// Even a partial install leaves the restored set consistent
			// (installAll puts back what it replaced), so restart either
			// way: the unit may be crash-looping on the failed release.
			err = errors.Join(err, a.activate(ctx, proj, back[0].rev))
		}
		result := resultRolledBack
		if err != nil {
			result = resultRollbackFailed
		}
		for _, r := range back {
			st.file(r.file).LastResult = result
		}
	}
	if len(stranded) > 0 {
		err = errors.Join(err, fmt.Errorf("no previous release of %v to roll back to; the failed release was left in place", stranded))
	}
	return err
}

// preflightFiles checks everything a files deploy needs before it
// changes anything.
func (a *Agent) preflightFiles(ctx context.Context, proj *Project, rels []fileRelease) error {
	for _, r := range rels {
		if _, err := os.Stat(blobPath(a.cfg.StateDir, proj.Name, r.hash, r.file.Name)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s isn't in the agent's store; deploy it from the build host (j-cicd run sends it)", r.ref())
			}
			return err
		}
		if err := checkWritable(filepath.Dir(r.file.Path)); err != nil {
			return fmt.Errorf("[%s] can't install into %s: %w", r.file.Name, filepath.Dir(r.file.Path), err)
		}
	}
	if proj.Systemd != nil {
		return a.checkUnitLoaded(ctx, proj.Systemd)
	}
	return nil
}

// reconcile compares what is on disk with what the state says was
// installed. A file the agent didn't put there (the release before the
// first deploy, or one replaced by hand since) is stored and becomes the
// release to roll back to, so even the first deploy can be undone and a
// hand edit is never silently lost.
func (a *Agent) reconcile(proj *Project, st *State, r fileRelease) error {
	ss := st.file(r.file)
	live, err := hashFile(r.file.Path)
	if errors.Is(err, fs.ErrNotExist) {
		if ss.Current != "" {
			a.log.Printf("[%s] warning: %s is missing; its recorded release %s is what a rollback restores", r.file.Name, r.file.Path, ss.Current)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("[%s] reading %s: %w", r.file.Name, r.file.Path, err)
	}
	if live == hexOf(ss.Current) {
		return nil
	}
	if ss.Current == "" {
		a.log.Printf("[%s] keeping the existing %s (sha256:%s) as the release to roll back to", r.file.Name, r.file.Path, shortID(live))
	} else {
		a.log.Printf("[%s] warning: %s was changed outside the agent (recorded %s, found sha256:%s); keeping the file found as the release to roll back to", r.file.Name, r.file.Path, ShortDigest(ss.Current), shortID(live))
	}
	src, err := os.Open(r.file.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := storeBlob(a.cfg.StateDir, proj.Name, live, r.file.Name, src); err != nil {
		return fmt.Errorf("[%s] keeping a copy of %s: %w", r.file.Name, r.file.Path, err)
	}
	tag := untrackedTag
	if live == r.hash {
		tag = r.rev
	}
	ss.Current, ss.CurrentTag = "sha256:"+live, tag
	return nil
}

// errRestored marks an install that failed part-way and put back the
// files it had already replaced: nothing was restarted, so nothing
// changed.
var errRestored = errors.New("the files already replaced were put back; nothing was restarted")

// installAll installs each release over its file. If one fails, the ones
// already replaced go back to what the state says was there.
func (a *Agent) installAll(proj *Project, st *State, rels []fileRelease) error {
	for i, r := range rels {
		a.log.Printf("[%s] installing %s at %s", r.file.Name, r.label(), r.file.Path)
		err := a.install(proj, r)
		if err == nil {
			continue
		}
		err = fmt.Errorf("[%s] installing %s: %w", r.file.Name, r.file.Path, err)
		var restoreErr error
		for _, done := range rels[:i] {
			if prev := hexOf(st.file(done.file).Current); prev != "" {
				restoreErr = errors.Join(restoreErr, a.install(proj, fileRelease{file: done.file, hash: prev}))
			}
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; putting back the files already replaced failed too: %v", err, restoreErr)
		}
		return fmt.Errorf("%w (%w)", err, errRestored)
	}
	return nil
}

// install replaces r's file with the stored release: copied beside the
// target (checked against its hash on the way), given its mode, synced,
// and renamed over the target, so the switch is atomic.
func (a *Agent) install(proj *Project, r fileRelease) error {
	src, err := os.Open(blobPath(a.cfg.StateDir, proj.Name, r.hash, r.file.Name))
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Dir(r.file.Path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(r.file.Path)+".j-cicd-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != r.hash {
		tmp.Close()
		return fmt.Errorf("the stored copy of sha256:%s is corrupt (it hashes to sha256:%s)", shortID(r.hash), shortID(got))
	}
	if err := tmp.Chmod(r.file.mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.file.Path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// activate restarts the unit (if any) and runs check: (if any).
func (a *Agent) activate(ctx context.Context, proj *Project, revision string) error {
	if proj.Systemd != nil {
		if err := a.restartUnit(ctx, proj); err != nil {
			return err
		}
	}
	if len(proj.Check) == 0 {
		return nil
	}
	if revision == untrackedTag {
		// A file found on disk has no known revision.
		revision = ""
	}
	cctx, cancel := context.WithTimeout(ctx, proj.healthTimeout())
	defer cancel()
	a.log.Printf("check: %s", strings.Join(proj.Check, " "))
	argv := append([]string{"J_CICD_PROJECT=" + proj.Name, "J_CICD_REVISION=" + revision}, proj.Check...)
	if err := a.runner.Run(cctx, "", "env", argv...); err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("check timed out after %s", proj.healthTimeout())
		}
		return fmt.Errorf("check failed: %w", err)
	}
	return nil
}

// receive reads a tar of artifacts and stores each in the project's blob
// store, refusing any file that isn't one of rels or doesn't hash to the
// sha256 its ref names. A ref whose file is already stored needn't be
// sent again.
func (a *Agent) receive(proj *Project, rels []fileRelease, r io.Reader) error {
	want := map[string]fileRelease{}
	for _, rel := range rels {
		want[rel.file.Name] = rel
	}
	got := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the artifacts sent: %w", err)
		}
		rel, ok := want[h.Name]
		switch {
		case !ok:
			return fmt.Errorf("%q was sent but isn't one of the files being deployed", h.Name)
		case got[h.Name]:
			return fmt.Errorf("%q was sent twice", h.Name)
		case h.Typeflag != tar.TypeReg:
			return fmt.Errorf("%q was sent as something other than a regular file", h.Name)
		case h.Size > maxArtifact:
			return fmt.Errorf("%q is %d bytes; the most the agent accepts is %d", h.Name, h.Size, int64(maxArtifact))
		}
		got[h.Name] = true
		if err := storeBlob(a.cfg.StateDir, proj.Name, rel.hash, rel.file.Name, tr); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		a.log.Printf("[%s] received %d bytes, sha256:%s verified", h.Name, h.Size, shortID(rel.hash))
	}
	for _, rel := range rels {
		if got[rel.file.Name] {
			continue
		}
		if _, err := os.Stat(blobPath(a.cfg.StateDir, proj.Name, rel.hash, rel.file.Name)); err != nil {
			return fmt.Errorf("%s was not sent", rel.ref())
		}
	}
	return nil
}

func blobDir(stateDir, project string) string {
	return filepath.Join(stateDir, project+".files")
}

// blobPath is where a release of a file is kept: by hash, then under the
// file's own name, so the copy is recognisable for what it is.
func blobPath(stateDir, project, hash, name string) string {
	return filepath.Join(blobDir(stateDir, project), hash, name)
}

// storeBlob saves r as the release of name with the given hash, failing
// (and keeping nothing) if the contents hash to anything else. Storing a
// release that is already there is a no-op.
func storeBlob(stateDir, project, hash, name string, r io.Reader) error {
	path := blobPath(stateDir, project, hash, name)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir := blobDir(stateDir, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		tmp.Close()
		return fmt.Errorf("contents hash to sha256:%s, not the sha256:%s it was sent as", shortID(got), shortID(hash))
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// pruneBlobs deletes stored releases other than each file's current and
// previous ones: nothing rolls back further than that.
func (s *State) pruneBlobs(stateDir string, proj *Project) error {
	keep := map[string]bool{}
	for _, f := range proj.Files {
		if ss := s.Services[f.Name]; ss != nil {
			keep[filepath.Join(hexOf(ss.Current), f.Name)] = true
			keep[filepath.Join(hexOf(ss.Previous), f.Name)] = true
		}
	}
	dir := blobDir(stateDir, proj.Name)
	hashes, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs error
	for _, h := range hashes {
		if !h.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, h.Name()))
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		left := len(files)
		for _, f := range files {
			if keep[filepath.Join(h.Name(), f.Name())] {
				continue
			}
			if err := os.Remove(filepath.Join(dir, h.Name(), f.Name())); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			left--
		}
		if left == 0 {
			errs = errors.Join(errs, os.Remove(filepath.Join(dir, h.Name())))
		}
	}
	return errs
}

// recordFilesUntouched records a deploy that stopped before changing
// anything.
func (a *Agent) recordFilesUntouched(proj *Project, st *State, rels []fileRelease, result string) {
	now := a.now().UTC()
	for _, r := range rels {
		ss := st.file(r.file)
		ss.LastAttempt, ss.LastDeploy, ss.LastResult = r.ref(), now, result
	}
	if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
		a.log.Printf("warning: recording the failed deploy: %v", err)
	}
}

// file returns the state for a file, creating it if needed.
func (s *State) file(f *File) *ServiceState {
	ss := s.Services[f.Name]
	if ss == nil {
		ss = &ServiceState{}
		s.Services[f.Name] = ss
	}
	return ss
}

// hashFile returns the hex sha256 of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkWritable reports whether a file can be created in dir, which is
// what an atomic replace needs (not write access to the file itself).
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".j-cicd-probe-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// syncDir makes a rename in dir durable. Best effort: not every
// filesystem supports syncing a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// hexOf strips the sha256: prefix the state records hashes with.
func hexOf(s string) string { return strings.TrimPrefix(s, "sha256:") }

func describeFiles(rs []fileRelease) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.file.Name + "=" + r.label()
	}
	return strings.Join(parts, ", ")
}

func shortFileLabel(r fileRelease) string {
	if r.rev != "" {
		return r.rev
	}
	return "sha256:" + shortID(r.hash)
}
