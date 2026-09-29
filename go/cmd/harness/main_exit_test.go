package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise main's actual parser and os.Exit boundary in an isolated child.
func TestEntrypointExitPolicy(t *testing.T) {
	if os.Getenv("LIP_ENTRYPOINT_CHILD") == "1" {
		os.Args = append([]string{"harness"}, os.Args[3:]...)
		main()
		return
	}
	missing := filepath.Join(t.TempDir(), "missing-qualification.json")
	for _, tc := range []struct {
		name    string
		argv    []string
		status  int
		message string
	}{
		{"manual refusal", nil, exitRefused, "no -config"},
		{"supervised refusal", []string{"-supervised"}, exitDrained, "no -config"},
		{"manual malformed", []string{"-badflag"}, exitRefused, "flag provided but not defined"},
		{"supervised malformed", []string{"-supervised", "-badflag"}, exitDrained, "flag provided but not defined"},
		{"marker later is manual", []string{"-badflag", "-supervised"}, exitRefused, "flag provided but not defined"},
		{"manual help", []string{"-help"}, exitDrained, "Usage of harness"},
		{"supervised help", []string{"-supervised", "-help"}, exitDrained, "Usage of harness"},
		{"supervised operational failure", []string{"-supervised", "-assess-qualification", missing}, exitFailed, "reading qualification evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestEntrypointExitPolicy$", "--"}, tc.argv...)...)
			cmd.Env = append(os.Environ(), "LIP_ENTRYPOINT_CHILD=1")
			out, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if tc.status == 0 {
				if err != nil {
					t.Fatalf("status: %v; output: %s", err, out)
				}
			} else if !ok || exit.ExitCode() != tc.status {
				t.Fatalf("status: %v, want %d; output: %s", err, tc.status, out)
			}
			if !strings.Contains(string(out), tc.message) {
				t.Fatalf("missing diagnostic %q: %s", tc.message, out)
			}
		})
	}

	t.Run("previously valid deployment now structurally refused", func(t *testing.T) {
		c, configPath, dir := newDeployFixture(t)
		if err := installAgent(c, configPath, agentOptions{Dir: dir, Rung: fixtureRung}, &bytes.Buffer{}); err != nil {
			t.Fatalf("install fixture: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(dir, agentLabel+".plist"))
		if err != nil {
			t.Fatal(err)
		}
		argv := harnessArgv(t, programArguments(t, string(body)))
		if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestEntrypointExitPolicy$", "--"}, argv...)...)
		cmd.Env = append(os.Environ(), "LIP_ENTRYPOINT_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("structural refusal must stop job with status 0: %v; %s", err, out)
		}
		if !strings.Contains(string(out), "harness:") {
			t.Fatalf("terminal refusal lacks diagnostic: %s", out)
		}
	})
}
