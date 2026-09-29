package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/rest"
)

func isStartupRefusal(err error) bool {
	var ref *refusal
	return errors.As(err, &ref)
}

func TestStartupFundingClassifiesCompleteTruthSeparatelyFromReadFailure(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	_, err := resolveFundingCap(context.Background(), c,
		&fundingScript{t: t, available: "20000", partial: true})
	if err == nil || isStartupRefusal(err) {
		t.Fatalf("incomplete account read must retry: %v", err)
	}
	base := &fundingScript{t: t, available: "20000"}
	doer := dispatchDoerFunc(func(ctx context.Context, req rest.Request) (rest.Response, error) {
		if req.Path == rest.EpPositions.Path {
			return rest.Response{Status: 200, Body: []byte(`{"cursor":"","market_positions":[{"ticker":"OTHER","position_fp":"1.00"}],"event_positions":[]}`)}, nil
		}
		return base.Do(ctx, req)
	})
	_, err = resolveFundingCap(context.Background(), c, doer)
	if !isStartupRefusal(err) {
		t.Fatalf("complete nonselected exposure must stop: %v", err)
	}
}

func TestStartupRigClassifiesAlertIOAndReleasesLock(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	factory := func(*hstore.Reader, *hstore.Store, time.Duration) (alertStepper, error) {
		return nil, fmt.Errorf("temporary alert initialization I/O")
	}
	_, err := newRig(context.Background(), h.cfg, false, h.xch, factory, newAnomalySink(), nil)
	if err == nil || isStartupRefusal(err) {
		t.Fatalf("alert initialization must retry: %v", err)
	}
	lock, err := acquireHarnessLock(h.cfg)
	if err != nil {
		t.Fatalf("startup error left the instance lock held: %v", err)
	}
	lock.Close()
	if err := os.Remove(h.cfg.Paths.DB); err != nil {
		t.Fatal(err)
	}
	_, err = newRig(context.Background(), h.cfg, false, h.xch, seamAlertFactory, newAnomalySink(), nil)
	if !isStartupRefusal(err) {
		t.Fatalf("absent operational DB must stop: %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(h.cfg.Paths.DB)); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestStartupMissingConfigRetriesAtEntrypoint(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestEntrypointExitPolicy$", "--", "-supervised", "-config", missing)
	cmd.Env = append(os.Environ(), "LIP_ENTRYPOINT_CHILD=1")
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != exitFailed {
		t.Fatalf("missing config must retry with status %d: %v; %s", exitFailed, err, output)
	}
}

func TestAlertDestinationEntrypointExitPolicy(t *testing.T) {
	configPath := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		goodTail(t)+`}`)
	c, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, env string
		write     bool
		status    int
	}{
		{"missing env retries", "", false, exitFailed},
		{"malformed topic stops", "NTFY_TOPIC=bad/topic\n", true, exitDrained},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.write {
				if err := os.WriteFile(c.Paths.Env, []byte(tc.env), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, supervised := range []bool{false, true} {
				name := "manual"
				argv := []string{"-config", configPath}
				want := tc.status
				if supervised {
					name = "supervised"
					argv = append([]string{"-supervised"}, argv...)
				} else if tc.status == exitDrained {
					want = exitRefused
				}
				t.Run(name, func(t *testing.T) {
					cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestEntrypointExitPolicy$", "--"}, argv...)...)
					cmd.Env = append(os.Environ(), "LIP_ENTRYPOINT_CHILD=1")
					output, err := cmd.CombinedOutput()
					if want == 0 {
						if err != nil {
							t.Fatalf("want status 0: %v; %s", err, output)
						}
						return
					}
					exit, ok := err.(*exec.ExitError)
					if !ok || exit.ExitCode() != want {
						t.Fatalf("want status %d: %v; %s", want, err, output)
					}
				})
			}
		})
	}
}
