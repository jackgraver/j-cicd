package main

import (
	"encoding/json"
	"os"

	"j-cicd/internal/store"
)

// jsonRun is a run as --json prints it: the stored record plus its
// duration, which isn't stored.
type jsonRun struct {
	store.Run
	DurationSeconds float64 `json:"duration_seconds"`
}

func toJSONRun(r store.Run) jsonRun {
	return jsonRun{Run: r, DurationSeconds: r.Duration().Round(100 * 1e6).Seconds()}
}

func toJSONRuns(runs []store.Run) []jsonRun {
	out := make([]jsonRun, len(runs))
	for i, r := range runs {
		out[i] = toJSONRun(r)
	}
	return out
}

// printJSON writes v to stdout as one line of JSON. Stdout carries only
// data and stderr only prose, so a script can parse the first without
// filtering the second.
func printJSON(v any) error {
	return json.NewEncoder(os.Stdout).Encode(v)
}
