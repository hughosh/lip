package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lip/harness/ping"
)

type fakeSender struct {
	calls int
	msg   ping.Message
	err   error
}

func (f *fakeSender) Send(_ context.Context, m ping.Message) error {
	f.calls++
	f.msg = m
	return f.err
}

type fakeCheckin struct {
	calls int
	err   error
}

func (f *fakeCheckin) CheckIn(context.Context) error { f.calls++; return f.err }

func TestModesAndSecretFreeReport(t *testing.T) {
	const secret = "secretTopicOrURL"
	secretErr := errors.New(secret)
	s := &fakeSender{}
	c := &fakeCheckin{}
	loadsTopic, loadsDead := 0, 0
	d := dependencies{
		loadTopic:   func(string) (ping.Topic, error) { loadsTopic++; return ping.Topic{}, nil },
		newSender:   func(ping.Topic) (sender, error) { return s, nil },
		loadDeadman: func(string) (checkin, error) { loadsDead++; return c, nil },
	}
	now := time.Date(2026, 9, 26, 8, 10, 11, 0, time.UTC)
	r := run(context.Background(), options{action: "check-config", env: "/fake", id: "Ab12"}, d, now)
	if !r.Success || loadsTopic != 1 || loadsDead != 1 || s.calls != 0 || c.calls != 0 {
		t.Fatalf("config called network or failed: %+v", r)
	}
	r = run(context.Background(), options{action: "send-test", env: "/fake", id: "Ab12"}, d, now)
	if !r.Success || s.calls != 1 || c.calls != 0 {
		t.Fatalf("test send: %+v", r)
	}
	if s.msg.Priority != ping.PriorityUrgent || !strings.Contains(s.msg.Body, "LIP READINESS TEST 2026-09-26T08:10:11Z/Ab12: test only; no trades started. Please acknowledge receipt in the active Codex chat.") {
		t.Fatalf("wrong message: %+v", s.msg)
	}
	r = run(context.Background(), options{action: "check-in", env: "/fake", id: "Ab12"}, d, now)
	if !r.Success || c.calls != 1 || s.calls != 1 {
		t.Fatalf("check-in: %+v", r)
	}
	s.err = secretErr
	r = run(context.Background(), options{action: "send-test", env: "/fake", id: "Ab12"}, d, now)
	if r.Success || r.ErrorClass != "delivery_error" {
		t.Fatalf("bad failure report: %+v", r)
	}
	p := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(p, r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "topic") || strings.Contains(string(b), "url") {
		t.Fatalf("report leaked secret: %s", b)
	}
}

func TestInvalidModesCannotSend(t *testing.T) {
	for _, args := range [][]string{
		{"-send-test"}, {"-check-in"}, {"-check-config", "-send-test", "-out", "/tmp/a"},
		{"-send-test", "-out", "/tmp/a", "-id", "bad/secret"},
		{"-send-test", "-out", "/tmp/a", "-env", "relative"},
	} {
		if _, err := parse(args, "/fake/env"); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
