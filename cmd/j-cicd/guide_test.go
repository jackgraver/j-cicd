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

// mentioned reports whether the guide names w as a command or flag: as a
// whitespace-separated word inside a backtick code span, so "list" doesn't
// match inside "lists" or "ui" inside "guide".
func mentioned(g, w string) bool {
	for _, span := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(g, -1) {
		for _, tok := range strings.Fields(span[1]) {
			if tok == w || strings.HasPrefix(tok, w+"=") {
				return true
			}
		}
	}
	return false
}

func TestGuideMentionsEverything(t *testing.T) {
	g := guide.Text()
	for _, w := range guideMustMention {
		if !mentioned(g, w) {
			t.Errorf("guide doesn't mention %q as a command or flag in backticks", w)
		}
	}
}

func TestMentionedIsWholeWord(t *testing.T) {
	g := "lists runs, a guide to build the version. `run --plan` and `logs <n> -f`."
	for _, w := range []string{"list", "ui", "version", "-v"} {
		if mentioned(g, w) {
			t.Errorf("%q matched inside prose", w)
		}
	}
	for _, w := range []string{"run", "--plan", "logs", "-f"} {
		if !mentioned(g, w) {
			t.Errorf("%q should match", w)
		}
	}
}

// Every subcommand in the usage text must appear in the guide, so adding a
// command without documenting it fails here.
func TestGuideCoversUsageCommands(t *testing.T) {
	src := usageText()
	g := guide.Text()
	for _, m := range regexp.MustCompile(`(?m)^  j-cicd (\w+)`).FindAllStringSubmatch(src, -1) {
		if !mentioned(g, m[1]) {
			t.Errorf("usage lists %q but the guide doesn't mention it", m[1])
		}
	}
}

func TestGuideCommand(t *testing.T) {
	if err := cmdGuide(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdGuide([]string{"-h"}); err != nil {
		t.Fatalf("-h should print help, got %v", err)
	}
	if err := cmdGuide([]string{"x"}); err == nil {
		t.Fatal("want usage error for extra argument")
	}
}
