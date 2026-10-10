// Command testsummary reads `go test -json` output on stdin and prints a
// per-package summary plus every failing or skipped test. It exits non-zero
// when any test or package failed, so CI fails on real failures only.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

type pkgStats struct {
	pass, fail, skip int
	elapsed          float64
	failed           bool
}

func main() {
	stats := map[string]*pkgStats{}
	var failures, skips []string
	output := map[string][]string{}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		ps := stats[e.Package]
		if ps == nil {
			ps = &pkgStats{}
			stats[e.Package] = ps
		}
		key := e.Package + " " + e.Test
		switch e.Action {
		case "output":
			if e.Test != "" {
				output[key] = append(output[key], e.Output)
			}
		case "pass":
			if e.Test != "" {
				ps.pass++
			} else {
				ps.elapsed = e.Elapsed
			}
		case "fail":
			if e.Test != "" {
				ps.fail++
				failures = append(failures, key)
			} else {
				ps.failed = true
				ps.elapsed = e.Elapsed
			}
		case "skip":
			if e.Test != "" {
				ps.skip++
				skips = append(skips, key)
			}
		}
	}
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	total := pkgStats{}
	fmt.Printf("%-50s %6s %6s %6s %8s\n", "PACKAGE", "PASS", "FAIL", "SKIP", "SECONDS")
	for _, n := range names {
		s := stats[n]
		if s.pass+s.fail+s.skip == 0 && !s.failed {
			continue
		}
		fmt.Printf("%-50s %6d %6d %6d %8.1f\n", n, s.pass, s.fail, s.skip, s.elapsed)
		total.pass += s.pass
		total.fail += s.fail
		total.skip += s.skip
		if s.failed {
			total.failed = true
		}
	}
	fmt.Printf("%-50s %6d %6d %6d\n", "TOTAL", total.pass, total.fail, total.skip)
	for _, k := range skips {
		fmt.Printf("SKIP %s\n", k)
	}
	for _, k := range failures {
		fmt.Printf("\nFAIL %s\n", k)
		for _, line := range output[k] {
			fmt.Print("    ", line)
		}
	}
	if total.fail > 0 || total.failed {
		os.Exit(1)
	}
}
