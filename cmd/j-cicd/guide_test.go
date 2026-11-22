package main

import (
	"regexp"
	"strings"
	"testing"

	"j-cicd/internal/guide"
)

// subcommands and flags every agent needs to know about. If one is added to
// the CLI without being listed here and in the guide, the guide has drifted.
var guideMustMention = []string{
	"run", "rollback", "add", "remove", "list", "history", "logs", "cancel",
	"init", "ui", "version", "guide",
	"--plan", "--json", "--no-push", "--no-deploy", "--deploy-dirty", "-v", "--quiet", "--tail",
}

func TestGuideMentionsEverything(t *testing.T) {
	g := guide.Text()
	for _, w := range guideMustMention {
		if !strings.Contains(g, w) {
			t.Errorf("guide doesn't mention %q", w)
		}
	}
}

// Every subcommand in the usage text must appear in the guide, so adding a
// command without documenting it fails here.
func TestGuideCoversUsageCommands(t *testing.T) {
	src := usageText()
	g := guide.Text()
	for _, m := range regexp.MustCompile(`(?m)^  j-cicd (\w+)`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(g, m[1]) {
			t.Errorf("usage lists %q but the guide doesn't mention it", m[1])
		}
	}
}

func TestGuideCommand(t *testing.T) {
	if err := cmdGuide(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdGuide([]string{"x"}); err == nil {
		t.Fatal("want usage error for extra argument")
	}
}
