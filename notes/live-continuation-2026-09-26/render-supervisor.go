// Render a temporary read-only supervision drill with production lifecycle code.
package main

import (
	"os"
	"lip/harness/lifecycle"
)

func main() {
	const root = "/Users/hugh/kek/lip/"
	const evidence = root + "notes/live-continuation-2026-09-26/"
	p := lifecycle.LaunchdPlan{
		Label: "com.lip.q01.20260926",
		Executable: root + "notes/live-readiness-2026-09-26/harness-candidate-v2",
		Args: []string{"-config", evidence + "q01-config.json", "-rung", "sizing", "-qualification", evidence + "q01-evidence.json"},
		WorkingDir: evidence + "q01-runtime",
		StdoutPath: evidence + "q01-supervised.out",
		StderrPath: evidence + "q01-supervised.err",
	}
	b, err := p.Render()
	if err != nil { panic(err) }
	if err := os.WriteFile(evidence + "q01-supervisor-rendered.plist", b, 0600); err != nil { panic(err) }
}
