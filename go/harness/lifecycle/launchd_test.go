package lifecycle

import (
	"encoding/xml"
	"strings"
	"testing"
)

func goodPlan() LaunchdPlan {
	return LaunchdPlan{
		Label:      "com.lip.harness",
		Executable: "/opt/lip/bin/harness",
		Args:       []string{"--config", "/opt/lip/etc/harness.json"},
		WorkingDir: "/opt/lip",
		StdoutPath: "/opt/lip/log/harness.out",
		StderrPath: "/opt/lip/log/harness.err",
	}
}

// TestLaunchdPlanUsesKeepAliveAndCaffeinateIS is H-DEP-2 and H-DEP-3.
//
// > | H-DEP-2 | **`launchd`, `KeepAlive: true`** | not `nohup`, not a terminal.
// > Restart on any exit, including exit 0. |
// > | H-DEP-3 | **`caffeinate`** | the launchd job execs `/usr/bin/caffeinate -is
// > <harness>`, so the assertion lives exactly as long as the process. The Mac
// > idle-sleeps and a sleep silently voids everything (F7). |
//
// `M-L-KEEPALIVE` renders `KeepAlive=false`, which turns the supervision that
// recovers from F18 into a one-shot launcher. `M-L-CAFFEINATE` drops the
// `caffeinate` wrapper or its `-is`, which returns the host to idle-sleeping
// under a live position -- the failure that voids the whole run silently.
func TestLaunchdPlanUsesKeepAliveAndCaffeinateIS(t *testing.T) {
	plan := goodPlan()
	out, err := plan.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := string(out)

	// The rendered plist must parse. A supervision config that does not load is
	// a harness with no KeepAlive at all, and nothing reports it.
	var probe any
	if err := xml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("the rendered plist is not well-formed XML: %v\n%s", err, text)
	}

	argv := plan.Command()
	want := []string{
		"/usr/bin/caffeinate", "-is", "/opt/lip/bin/harness",
		"--config", "/opt/lip/etc/harness.json",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv %v, want %v", describe(argv), describe(want))
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)",
				i, argv[i], want[i], describe(argv))
		}
	}

	// KeepAlive and RunAtLoad are asserted as key-then-true PAIRS, not merely as
	// keys that exist somewhere: `M-L-KEEPALIVE` renders the key with a false
	// value, which a key-presence check would pass.
	//
	// The EMPTY-ELEMENT form is asserted, and that is `lip-83o`. launchd's plist
	// parser accepts only `<true/>` and rejects `<true></true>` with
	// `Bootstrap failed: 5: Input/output error`, so for the whole life of this
	// code `-deploy` produced a plist that could never be loaded. This
	// assertion used to require the PAIRED form, which is how the defect was
	// pinned in place rather than caught.
	for _, key := range []string{"KeepAlive", "RunAtLoad"} {
		pair := "<key>" + key + "</key>\n\t<true/>"
		if !strings.Contains(text, pair) {
			t.Fatalf("%s is not rendered true. H-DEP-2 restarts on ANY exit, "+
				"including exit 0 -- the thing that stops a drained harness "+
				"coming straight back up is the durable latch on disk, not a "+
				"supervisor that declined to restart it:\n%s", key, text)
		}
	}
	// The bytes, and nothing but the bytes. Every other plist parser accepts
	// the paired form -- `plutil -lint` reports OK, PlistBuddy reads every key,
	// and a Go XML or plist decoder round-trips it -- so a test that decodes
	// this output passes either way. The only parser that rejects it is reached
	// by actually calling `launchctl bootstrap`, which no test does.
	if strings.Contains(text, "<true></true>") {
		t.Fatalf("the plist emits the PAIRED boolean form. launchd accepts "+
			"only <true/> and fails the load with `Bootstrap failed: 5: "+
			"Input/output error`, which names nothing and points nowhere "+
			"(lip-83o). Do not assert this by decoding the output: every "+
			"decoder accepts both forms.\n%s", text)
	}
	if strings.Contains(text, "<false") {
		t.Fatalf("the plist contains a false value:\n%s", text)
	}
	for _, key := range []string{"Label", "ProgramArguments", "WorkingDirectory",
		"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(text, "<key>"+key+"</key>") {
			t.Fatalf("the plist has no %s key:\n%s", key, text)
		}
	}

	// caffeinate is first in argv and carries BOTH flags. `-i` alone leaves the
	// machine free to sleep on the power-management path that actually fires
	// overnight on this host.
	wantArgv := "<array>\n\t\t<string>/usr/bin/caffeinate</string>\n\t\t" +
		"<string>-is</string>\n\t\t<string>/opt/lip/bin/harness</string>"
	if !strings.Contains(text, wantArgv) {
		t.Fatalf("ProgramArguments does not exec `/usr/bin/caffeinate -is "+
			"<harness>`; the power assertion must live exactly as long as the "+
			"process (H-DEP-3):\n%s", text)
	}

	// Rendering writes nothing. Installation is `lip-3af`.
	if strings.Contains(text, "LaunchAgents") {
		t.Fatal("Render referenced an install location")
	}
}

// TestLaunchdPlanRefusesWrappersAndRelativePaths is H-DEP-2's "not `nohup`, not
// a terminal", stated as a rejection rather than a convention.
//
// A wrapper becomes the process launchd supervises, so `KeepAlive` watches the
// wrapper's exit rather than the harness's and `caffeinate`'s assertion is scoped
// to the wrapper too. A harness that panics under `sh -c` leaves a shell that
// exits cleanly -- and `KeepAlive` on a clean exit is exactly the case that would
// not bring it back.
func TestLaunchdPlanRefusesWrappersAndRelativePaths(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LaunchdPlan)
	}{
		{"no label", func(p *LaunchdPlan) { p.Label = "" }},
		{"relative executable", func(p *LaunchdPlan) { p.Executable = "bin/harness" }},
		{"relative working dir", func(p *LaunchdPlan) { p.WorkingDir = "." }},
		{"relative stdout", func(p *LaunchdPlan) { p.StdoutPath = "out.log" }},
		{"relative stderr", func(p *LaunchdPlan) { p.StderrPath = "err.log" }},
		{"empty stdout", func(p *LaunchdPlan) { p.StdoutPath = "" }},
		{"nohup wrapper", func(p *LaunchdPlan) {
			p.Args = append([]string{p.Executable}, p.Args...)
			p.Executable = "/usr/bin/nohup"
		}},
		{"shell wrapper", func(p *LaunchdPlan) { p.Executable = "/bin/sh" }},
		{"shell in args", func(p *LaunchdPlan) { p.Args = []string{"/bin/bash"} }},
		{"env wrapper", func(p *LaunchdPlan) { p.Executable = "/usr/bin/env" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := goodPlan()
			tc.mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", plan)
			}
			if _, err := plan.Render(); err == nil {
				t.Fatalf("Render accepted %+v", plan)
			}
		})
	}
}

// TestLaunchdPlanEscapesPathsThroughTheEncoder is why this uses encoding/xml
// rather than fmt.Sprintf.
//
// A path containing `&` or `<` silently produces a plist launchd refuses to
// load, and the failure mode is that the supervision mechanism is simply absent
// with nothing reporting it.
func TestLaunchdPlanEscapesPathsThroughTheEncoder(t *testing.T) {
	plan := goodPlan()
	plan.Label = "com.lip.harness&test"
	plan.StdoutPath = "/opt/lip/log/a<b>&c/harness.out"

	out, err := plan.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := xml.Unmarshal(out, new(any)); err != nil {
		t.Fatalf("a path with XML metacharacters produced a malformed plist, "+
			"which launchd refuses to load -- leaving the harness with no "+
			"KeepAlive and nothing reporting it: %v\n%s", err, out)
	}
	text := string(out)
	if strings.Contains(text, "a<b>&c") {
		t.Fatalf("the path was interpolated raw rather than encoded:\n%s", text)
	}
	if !strings.Contains(text, "&amp;") {
		t.Fatalf("no escaped ampersand in:\n%s", text)
	}
}
