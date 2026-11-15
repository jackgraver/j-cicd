package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"j-cicd/internal/config"
)

// buildImage shells out to `docker build`, tagging the image once per
// entry in img.Tags.
func (p *Pipeline) buildImage(ctx context.Context, timeout time.Duration, img *config.Image) error {
	buildContext := filepath.Join(p.projectDir, img.Context)
	dockerfile := filepath.Join(p.projectDir, img.Dockerfile)

	args := []string{"build", "-f", dockerfile}
	for _, tag := range img.Tags {
		args = append(args, "-t", ref(img, tag))
	}
	args = append(args, buildContext)

	p.log.Printf("[%s] docker %s", img.Label(), strings.Join(args, " "))
	return p.exec(ctx, timeout, "", "docker", args...)
}

// pushImage shells out to `docker push` once per configured tag.
func (p *Pipeline) pushImage(ctx context.Context, timeout time.Duration, img *config.Image) error {
	for _, tag := range img.Tags {
		r := ref(img, tag)
		p.log.Printf("[%s] docker push %s", img.Label(), r)
		if err := p.exec(ctx, timeout, "", "docker", "push", r); err != nil {
			return fmt.Errorf("pushing %s: %w", r, err)
		}
	}
	return nil
}

func ref(img *config.Image, tag string) string {
	return img.Repository + ":" + tag
}
