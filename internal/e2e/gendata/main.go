// Command gendata generates a standalone dummy photo/video dataset for manual
// GUI testing of PAIM. It reuses the exact fixture builders the e2e scenario test
// uses (internal/e2e), so the hand-testing data matches what the automated test
// exercises.
//
// Usage:
//
//	go run ./internal/e2e/gendata <target-dir>
//
// It writes ~15 EXIF JPEGs across three dates, two exact duplicates, a RAW+JPEG
// pair, a video, and a hidden junk file into <target-dir>.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/Sam-Lam/PAIM/internal/e2e"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gendata <target-dir>")
		os.Exit(2)
	}
	target := os.Args[1]

	bin := lookupExiftool()
	if bin == "" {
		fmt.Fprintln(os.Stderr, "error: exiftool not found on PATH or at /opt/homebrew/bin/exiftool")
		os.Exit(1)
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create %q: %v\n", target, err)
		os.Exit(1)
	}

	tree, err := e2e.BuildLargeDataset(target, bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: build dataset: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Generated dummy dataset at %s\n", target)
	fmt.Printf("  media files: %d (%d duplicates)\n", tree.MediaCount(), tree.DuplicateCount())
	fmt.Printf("  junk files:  %d\n", len(tree.JunkPaths))

	// Summarize by extension for a quick sanity check.
	byExt := map[string]int{}
	for _, f := range tree.Media {
		byExt[f.Ext]++
	}
	exts := make([]string, 0, len(byExt))
	for e := range byExt {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	for _, e := range exts {
		fmt.Printf("    .%-4s %d\n", e, byExt[e])
	}

	fmt.Println("Files:")
	for _, f := range tree.Media {
		rel, _ := filepath.Rel(target, f.Path)
		tag := ""
		if f.IsDuplicate {
			tag = "  [duplicate]"
		} else if f.FromMtime {
			tag = "  [no EXIF -> mtime]"
		}
		fmt.Printf("  %s%s\n", rel, tag)
	}
}

// lookupExiftool resolves the exiftool binary, preferring PATH.
func lookupExiftool() string {
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p
	}
	const brew = "/opt/homebrew/bin/exiftool"
	if _, err := os.Stat(brew); err == nil {
		return brew
	}
	return ""
}
