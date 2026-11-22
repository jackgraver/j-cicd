package pipeline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"j-cicd/internal/config"
)

// DefaultVerifyInterval is the pause between attempts of a verify check.
const DefaultVerifyInterval = 3 * time.Second

// VerifyResult is a verify check that passed.
type VerifyResult struct {
	URL      string
	Attempts int
	Duration time.Duration
}

// Verified returns the checks the last Run passed.
func (p *Pipeline) Verified() []VerifyResult { return p.verified }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func expandRevision(s, revision string) string {
	return strings.ReplaceAll(s, config.RevisionPlaceholder, revision)
}

// verify runs the config's post-deploy checks in order. The agent has
// already said the containers are healthy; this asks the public site
// whether it is actually serving the release, which a stale cache or a
// proxy pointing at old containers would still get wrong.
func (p *Pipeline) verify(ctx context.Context, cfg *config.Config) error {
	for i := range cfg.Deploy.Verify {
		v := &cfg.Deploy.Verify[i]
		target := expandRevision(v.URL, p.revision)
		// Each check is a stage of its own, named by the URL it asks, so a
		// failure and its log marker say which check it was.
		p.stage(KindVerify, target)
		p.say("verifying %s", target)
		started := time.Now()
		p.log.Printf("[verify] GET %s (expect status %d%s)", target, v.ExpectStatus, expectation(v, p.revision))
		attempts, err := p.verifyOne(ctx, v, target)
		if err != nil {
			return fmt.Errorf("verify %s failed: %w", target, err)
		}
		took := time.Since(started)
		p.verified = append(p.verified, VerifyResult{URL: target, Attempts: attempts, Duration: took})
		p.log.Printf("[verify] %s is serving revision %s (%s, %s)", target, p.revision, plural(attempts, "attempt"), took.Round(time.Millisecond))
		p.say("verified %s (%s, %s)", target, plural(attempts, "attempt"), took.Round(100*time.Millisecond))
	}
	return nil
}

func expectation(v *config.Verify, revision string) string {
	if v.ExpectBodyContains == "" {
		return ""
	}
	return fmt.Sprintf(", body containing %q", expandRevision(v.ExpectBodyContains, revision))
}

// verifyOne retries one check until it passes or its timeout expires.
// It returns how many requests it took.
func (p *Pipeline) verifyOne(ctx context.Context, v *config.Verify, target string) (int, error) {
	retryFor := v.RetryFor()
	deadline := time.Now().Add(retryFor)
	var last string
	for attempt := 1; ; attempt++ {
		ok, detail, err := p.checkOnce(ctx, v, target)
		if err != nil {
			return attempt, err // cancelled
		}
		if ok {
			return attempt, nil
		}
		last = detail
		if !time.Now().Add(p.verifyEvery).Before(deadline) {
			return attempt, fmt.Errorf("still not serving this release after %d attempt(s) over %s: %s", attempt, retryFor, last)
		}
		p.log.Printf("[verify] attempt %d: %s; retrying", attempt, last)
		select {
		case <-ctx.Done():
			return attempt, ctx.Err()
		case <-time.After(p.verifyEvery):
		}
	}
}

// checkOnce makes one request. It reports whether the response was what
// was expected and, if not, why; err is only for a cancelled run. A
// request that fails to connect is just a failed attempt.
func (p *Pipeline) checkOnce(ctx context.Context, v *config.Verify, target string) (ok bool, detail string, err error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	u, perr := url.Parse(target)
	if perr != nil {
		return false, "", perr
	}
	// A cache-busting parameter, so an intermediary that keys on the full
	// URL can't answer from an old copy of this one.
	q := u.Query()
	q.Set("_jcicd", strconv.FormatInt(time.Now().UnixNano(), 36))
	u.RawQuery = q.Encode()

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if rerr != nil {
		return false, "", rerr
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	resp, derr := p.httpClient.Do(req)
	if derr != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		return false, "request failed: " + derr.Error(), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != v.ExpectStatus {
		return false, fmt.Sprintf("got status %d, want %d", resp.StatusCode, v.ExpectStatus), nil
	}
	if want := expandRevision(v.ExpectBodyContains, p.revision); want != "" && !strings.Contains(string(body), want) {
		return false, fmt.Sprintf("body doesn't contain %q (got %q)", want, snippet(body)), nil
	}
	return true, "", nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
