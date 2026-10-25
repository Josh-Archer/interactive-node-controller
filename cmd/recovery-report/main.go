// Command recovery-report checks a desktop recovery evidence bundle.
// It exits non-zero when detection, admission, eviction acceptance, and
// resource release are inconsistent or when a Kind report claims performance.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Josh-Archer/interactive-node-controller/internal/recovery"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "-report" {
		fmt.Fprintln(os.Stderr, "usage: recovery-report -report path.json")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read report: %v\n", err)
		os.Exit(1)
	}
	var report recovery.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		fmt.Fprintf(os.Stderr, "decode report: %v\n", err)
		os.Exit(1)
	}
	problems := recovery.Evaluate(report)
	if len(problems) != 0 {
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, problem)
		}
		os.Exit(1)
	}
	fmt.Printf("report accepted environment=%s performanceClaimed=%v scenarios=%d\n", report.Environment, report.PerformanceClaimed, len(report.Scenarios))
}
