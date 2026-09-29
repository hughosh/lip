// alarmcheck is an attended, single-action diagnostic for the external alarms.
// It never starts the harness or schedules a heartbeat.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lip/harness/ping"
)

type sender interface {
	Send(context.Context, ping.Message) error
}
type checkin interface{ CheckIn(context.Context) error }
type dependencies struct {
	loadTopic   func(string) (ping.Topic, error)
	newSender   func(ping.Topic) (sender, error)
	loadDeadman func(string) (checkin, error)
}

var production = dependencies{
	loadTopic:   ping.LoadNTFYTopic,
	newSender:   func(t ping.Topic) (sender, error) { return ping.NewNTFYSender(t) },
	loadDeadman: func(path string) (checkin, error) { return ping.LoadHTTPSDeadman(path) },
}

type report struct {
	UTC        string `json:"utc"`
	ID         string `json:"id"`
	Action     string `json:"action"`
	Success    bool   `json:"success"`
	ErrorClass string `json:"error_class,omitempty"`
}

type options struct {
	action, env, out, id string
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func parse(args []string, defaultEnv string) (options, error) {
	f := flag.NewFlagSet("alarmcheck", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	checkConfig := f.Bool("check-config", false, "validate alarm configuration without network calls (default)")
	sendTest := f.Bool("send-test", false, "send one test notification")
	checkIn := f.Bool("check-in", false, "send one external dead-man check-in")
	env := f.String("env", defaultEnv, "absolute env-file path")
	out := f.String("out", "", "JSON report path; required for network modes")
	id := f.String("id", "", "alphanumeric run ID")
	if err := f.Parse(args); err != nil {
		return options{}, err
	}
	if f.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments")
	}
	n := 0
	for _, b := range []bool{*checkConfig, *sendTest, *checkIn} {
		if b {
			n++
		}
	}
	if n > 1 {
		return options{}, fmt.Errorf("select exactly one mode")
	}
	action := "check-config"
	if *sendTest {
		action = "send-test"
	}
	if *checkIn {
		action = "check-in"
	}
	if action != "check-config" && *out == "" {
		return options{}, fmt.Errorf("-out is required for network modes")
	}
	if !filepath.IsAbs(*env) {
		return options{}, fmt.Errorf("-env must be an absolute path")
	}
	if *id != "" && !validID(*id) {
		return options{}, fmt.Errorf("-id must contain 1-64 ASCII letters or digits")
	}
	return options{action: action, env: *env, out: *out, id: *id}, nil
}

// run makes at most one external request. Errors from the transport and loaders
// are classified without using their text, which may contain a bearer URL.
func run(ctx context.Context, o options, d dependencies, now time.Time) report {
	utc := now.UTC()
	id := o.id
	if id == "" {
		id = utc.Format("20060102T150405000000000")
	}
	r := report{UTC: utc.Format(time.RFC3339Nano), ID: id, Action: o.action}
	switch o.action {
	case "check-config":
		_, topicErr := d.loadTopic(o.env)
		_, deadErr := d.loadDeadman(o.env)
		if topicErr != nil || deadErr != nil {
			r.ErrorClass = "config_error"
			return r
		}
	case "send-test":
		topic, err := d.loadTopic(o.env)
		if err != nil {
			r.ErrorClass = "config_error"
			return r
		}
		s, err := d.newSender(topic)
		if err != nil {
			r.ErrorClass = "config_error"
			return r
		}
		body := "LIP READINESS TEST " + utc.Format(time.RFC3339) + "/" + id + ": test only; no trades started. Please acknowledge receipt in the active Codex chat."
		if err := s.Send(ctx, ping.Message{Title: "LIP readiness test", Body: body, Priority: ping.PriorityUrgent}); err != nil {
			r.ErrorClass = "delivery_error"
			return r
		}
	case "check-in":
		dead, err := d.loadDeadman(o.env)
		if err != nil {
			r.ErrorClass = "config_error"
			return r
		}
		if err := dead.CheckIn(ctx); err != nil {
			r.ErrorClass = "checkin_error"
			return r
		}
	default:
		r.ErrorClass = "invalid_mode"
		return r
	}
	r.Success = true
	return r
}

func writeReport(path string, r report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if path == "" {
		_, err = fmt.Fprintln(os.Stdout, string(b))
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not locate home directory")
		os.Exit(2)
	}
	o, err := parse(os.Args[1:], filepath.Join(home, ".kalshi", "env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid options:", err)
		os.Exit(2)
	}
	// In particular, no raw loader or transport error is printed.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := run(ctx, o, production, time.Now())
	if err := writeReport(o.out, r); err != nil {
		fmt.Fprintln(os.Stderr, "could not write report")
		os.Exit(1)
	}
	if !r.Success {
		fmt.Fprintln(os.Stderr, strings.ReplaceAll(r.ErrorClass, "_", " "))
		os.Exit(1)
	}
}

// confidence: high
