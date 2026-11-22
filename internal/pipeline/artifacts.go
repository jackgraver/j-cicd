package pipeline

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"j-cicd/internal/config"
)

// ArtifactResult is an artifact Run found after the steps.
type ArtifactResult struct {
	Name string
	// Path is relative to the project dir.
	Path string
	// SHA256 is the hex sha256 of the file: what identifies this release
	// of it to the agent, as a digest identifies an image.
	SHA256 string
	Size   int64
	// path is the file on disk, for sending it.
	path string
}

// Ref is how a deploy names the artifact to the agent: name@sha256:<hex>.
func (a ArtifactResult) Ref() string { return a.Name + "@sha256:" + a.SHA256 }

// Artifacts returns the artifacts the last Run checked, in artifacts:
// order.
func (p *Pipeline) Artifacts() []ArtifactResult { return p.artifacts }

// collectArtifacts checks that the steps produced every artifact and
// hashes each one. A missing artifact fails the run here, by name, rather
// than as a confusing error halfway through a deploy.
func (p *Pipeline) collectArtifacts(cfg *config.Config) error {
	for _, a := range cfg.Artifacts {
		path := filepath.Join(p.projectDir, a.Path)
		sum, size, err := hashFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("artifact %s: the steps didn't produce %s", a.Name, a.Path)
			}
			return fmt.Errorf("artifact %s: %w", a.Name, err)
		}
		p.log.Printf("[artifact] %s: %s (%d bytes, sha256:%s)", a.Name, a.Path, size, sum[:12])
		p.artifacts = append(p.artifacts, ArtifactResult{Name: a.Name, Path: a.Path, SHA256: sum, Size: size, path: path})
	}
	return nil
}

// hashFile returns the hex sha256 and size of a regular file.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// deployedArtifacts returns the artifacts the deploy sends, in order.
func (p *Pipeline) deployedArtifacts(cfg *config.Config) []ArtifactResult {
	var out []ArtifactResult
	for _, want := range cfg.DeployArtifacts() {
		for _, a := range p.artifacts {
			if a.Name == want.Name {
				out = append(out, a)
			}
		}
	}
	return out
}

// artifactStream returns a tar of arts, one entry per artifact named by
// its artifact name, written as it's read. The agent checks each entry
// against the sha256 the deploy names it by, so a file that changed
// after it was hashed is refused there rather than installed.
func artifactStream(arts []ArtifactResult) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeArtifacts(pw, arts))
	}()
	return pr
}

func writeArtifacts(w io.Writer, arts []ArtifactResult) error {
	tw := tar.NewWriter(w)
	for _, a := range arts {
		if err := writeArtifact(tw, a); err != nil {
			return fmt.Errorf("sending artifact %s: %w", a.Name, err)
		}
	}
	return tw.Close()
}

func writeArtifact(tw *tar.Writer, a ArtifactResult) error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: a.Name, Size: info.Size(), Mode: 0o644, ModTime: info.ModTime(),
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
