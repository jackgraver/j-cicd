package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// buildImage shells out to `docker build`, tagging the image once per entry
// in img.Tags.
func buildImage(projectDir string, img *ImageConfig) error {
	context := filepath.Join(projectDir, img.Context)
	dockerfile := filepath.Join(projectDir, img.Dockerfile)

	args := []string{"build", "-f", dockerfile}
	for _, tag := range img.Tags {
		args = append(args, "-t", fmt.Sprintf("%s:%s", img.Repository, tag))
	}
	args = append(args, context)

	log.Printf("[%s] docker %s", img.Label(), strings.Join(args, " "))
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// pushImage shells out to `docker push` once per configured tag.
func pushImage(img *ImageConfig) error {
	for _, tag := range img.Tags {
		ref := fmt.Sprintf("%s:%s", img.Repository, tag)
		log.Printf("[%s] docker push %s", img.Label(), ref)
		cmd := exec.Command("docker", "push", ref)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pushing %s: %w", ref, err)
		}
	}
	return nil
}
