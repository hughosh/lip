package lifecycle

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

// caffeinatePath and caffeinateFlags are H-DEP-3, pinned.
//
// > **`caffeinate`** | the launchd job execs `/usr/bin/caffeinate -is
// > <harness>`, so the assertion lives exactly as long as the process. The Mac
// > idle-sleeps and a sleep silently voids everything (F7).
//
// `-i` prevents idle sleep and `-s` prevents system sleep on AC power. Both, and
// the difference matters: `-i` alone leaves the machine free to sleep on the
// power-management path that actually fires overnight on this host.
const (
	caffeinatePath  = "/usr/bin/caffeinate"
	caffeinateFlags = "-is"
)

// LaunchdPlan is the supervision configuration of H-DEP-2 and H-DEP-3.
//
// It is RENDERED here and installed in `lip-3af`, when the executable's final
// path exists. Rendering separately from installing is what lets the negative
// control break it: `M-L-KEEPALIVE` and `M-L-CAFFEINATE` mutate a pure function
// whose output a test can read, where a plist written straight to
// `~/Library/LaunchAgents` would be a side effect no gate inspects.
type LaunchdPlan struct {
	Label      string
	Executable string
	Args       []string
	WorkingDir string
	StdoutPath string
	StderrPath string
}

// plistEntry and plistDict render a plist through encoding/xml.
//
// The encoder rather than string interpolation is not stylistic. A path
// containing `&` or `<` -- which a log path under a directory named for a market
// can easily have -- silently produces a plist that `launchd` refuses to load,
// and a supervision config that does not load is a harness with no `KeepAlive`
// at all. The failure is that the safety mechanism is absent, and nothing
// reports it.
type plistEntry struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type plistArray struct {
	XMLName xml.Name     `xml:"array"`
	Items   []plistEntry `xml:"string"`
}

// Validate rejects every configuration that would supervise the wrong thing, or
// nothing.
func (p LaunchdPlan) Validate() error {
	if p.Label == "" {
		return errors.New("launchd plan has no Label; launchctl identifies the " +
			"job by it and an empty one cannot be loaded, unloaded or queried")
	}
	for name, path := range map[string]string{
		"Executable": p.Executable,
		"WorkingDir": p.WorkingDir,
		"StdoutPath": p.StdoutPath,
		"StderrPath": p.StderrPath,
	} {
		if path == "" {
			return fmt.Errorf("launchd plan has no %s", name)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("launchd plan %s %q is not absolute: launchd runs "+
				"the job with no shell and no inherited working directory, so a "+
				"relative path is resolved against something neither the "+
				"operator nor this code chose", name, path)
		}
	}
	if err := rejectShell(p.Executable); err != nil {
		return err
	}
	for _, a := range p.Args {
		if err := rejectShell(a); err != nil {
			return err
		}
	}
	return nil
}

// rejectShell refuses `nohup` and any shell wrapper.
//
// H-DEP-2: "not `nohup`, not a terminal." The reason is specific rather than
// stylistic: a wrapper becomes the process `launchd` supervises, so `KeepAlive`
// watches the wrapper's exit rather than the harness's, and `caffeinate`'s
// assertion is scoped to the wrapper too. A harness that panics under `sh -c`
// leaves a shell that exits cleanly, and `KeepAlive` on a clean exit is exactly
// the thing that would not restart it.
func rejectShell(arg string) error {
	base := filepath.Base(arg)
	switch base {
	case "nohup", "sh", "bash", "zsh", "env", "open", "script":
		return fmt.Errorf("launchd plan references %q: H-DEP-2 requires launchd "+
			"supervise the harness directly, because a wrapper is the process "+
			"KeepAlive would watch and caffeinate's assertion would be scoped "+
			"to", arg)
	}
	return nil
}

// Command is the effective argv: `/usr/bin/caffeinate -is <harness> [args...]`.
//
// `caffeinate` execs the harness rather than spawning it, so there is exactly
// one process and the power assertion lives exactly as long as it. That is why
// this is one argv and not a `caffeinate` job plus a harness job: two jobs can
// be alive in either order, and the interesting window is the one where the
// harness is up and the assertion is not.
func (p LaunchdPlan) Command() []string {
	argv := []string{caffeinatePath, caffeinateFlags, p.Executable}
	return append(argv, p.Args...)
}

// Render produces the plist XML. It writes nothing: installation is `lip-3af`.
func (p LaunchdPlan) Render() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	var body bytes.Buffer
	enc := xml.NewEncoder(&body)
	enc.Indent("\t", "\t")

	emitKey := func(k string) error {
		return enc.Encode(plistEntry{XMLName: xml.Name{Local: "key"}, Value: k})
	}
	emitString := func(v string) error {
		return enc.Encode(plistEntry{XMLName: xml.Name{Local: "string"}, Value: v})
	}
	// `lip-83o`. launchd's own plist parser accepts ONLY the empty-element form
	// `<true/>` and rejects the paired `<true></true>` outright, failing the
	// load with the uninformative `Bootstrap failed: 5: Input/output error`.
	// Go's encoding/xml has no way to emit an empty element -- it always writes
	// the paired form -- so this one token is written directly, after flushing
	// the encoder so the two writers stay in order.
	//
	// EVERY OTHER PLIST PARSER ACCEPTS THE PAIRED FORM, which is why this
	// survived: `plutil -lint` reports OK, PlistBuddy reads every key, and any
	// test that round-trips this output through an XML or plist decoder passes.
	// The only parser that rejects it is reachable solely by actually calling
	// `launchctl bootstrap`. So the test for this asserts the rendered BYTES.
	emitTrue := func() error {
		if err := enc.Flush(); err != nil {
			return err
		}
		_, err := body.WriteString("\n\t<true/>")
		return err
	}
	emitFalse := func() error {
		if err := enc.Flush(); err != nil {
			return err
		}
		_, err := body.WriteString("\n\t<false/>")
		return err
	}

	if err := emitKey("Label"); err != nil {
		return nil, err
	}
	if err := emitString(p.Label); err != nil {
		return nil, err
	}

	if err := emitKey("ProgramArguments"); err != nil {
		return nil, err
	}
	arr := plistArray{}
	for _, a := range p.Command() {
		arr.Items = append(arr.Items,
			plistEntry{XMLName: xml.Name{Local: "string"}, Value: a})
	}
	if err := enc.Encode(arr); err != nil {
		return nil, err
	}

	// Restart failed/crashed runs, but leave planned drains and supervised
	// structural refusals stopped (H-DEP-2).
	if err := emitKey("KeepAlive"); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	if _, err := body.WriteString("\n\t<dict>"); err != nil {
		return nil, err
	}
	if err := emitKey("SuccessfulExit"); err != nil {
		return nil, err
	}
	if err := emitFalse(); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	if _, err := body.WriteString("\n\t</dict>"); err != nil {
		return nil, err
	}

	if err := emitKey("RunAtLoad"); err != nil {
		return nil, err
	}
	if err := emitTrue(); err != nil {
		return nil, err
	}
	if err := emitKey("ThrottleInterval"); err != nil {
		return nil, err
	}
	if err := enc.Encode(plistEntry{XMLName: xml.Name{Local: "integer"}, Value: strconv.Itoa(60)}); err != nil {
		return nil, err
	}

	for _, kv := range [][2]string{
		{"WorkingDirectory", p.WorkingDir},
		{"StandardOutPath", p.StdoutPath},
		{"StandardErrorPath", p.StderrPath},
	} {
		if err := emitKey(kv[0]); err != nil {
			return nil, err
		}
		if err := emitString(kv[1]); err != nil {
			return nil, err
		}
	}

	if err := enc.Flush(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	out.WriteString("<plist version=\"1.0\">\n<dict>\n\t")
	out.Write(bytes.TrimLeft(body.Bytes(), "\n\t"))
	out.WriteString("\n</dict>\n</plist>\n")
	return out.Bytes(), nil
}

// confidence: high
