package ping

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ntfyBase is the FIXED production destination. It is a constant and not a
// parameter: a configurable notification host is a configurable place for every
// alert to go instead, and nothing in §13 needs one.
const ntfyBase = "https://ntfy.sh"

// Priority values, in ntfy's own vocabulary. SEV1 is `urgent` because that is
// what makes a phone ring through a silent profile, which is the entire
// difference between a 3am alert and a 9am one.
const (
	PriorityUrgent  = "urgent"
	PriorityDefault = "default"
)

// Message is one push. It carries no topic, deliberately: the topic is the
// destination and never the content.
type Message struct {
	Title    string
	Body     string
	Priority string
}

// NTFYSender delivers to one topic over HTTPS, without following redirects.
type NTFYSender struct {
	topic Topic
	base  string
	http  *http.Client
}

// NewNTFYSender builds the production sender.
func NewNTFYSender(t Topic) (*NTFYSender, error) {
	return newNTFYSenderAt(ntfyBase, t)
}

// newNTFYSenderAt is the same sender pointed at an arbitrary base, and it is
// UNEXPORTED so that only this package's tests can move the destination. An
// exported version would be a supported way to send every alert somewhere else.
func newNTFYSenderAt(base string, t Topic) (*NTFYSender, error) {
	if !t.Valid() {
		return nil, errors.New("no ntfy topic: §13 has no fallback channel, " +
			"and a sender without a topic delivers nothing while reporting no " +
			"error at every call site that forgot to check")
	}
	if base == "" {
		return nil, errors.New("no ntfy base URL")
	}
	return &NTFYSender{topic: t, base: strings.TrimSuffix(base, "/"),
		http: newBearerClient()}, nil
}

// valid reports whether this sender was built by a constructor.
//
// Go permits `&ping.NTFYSender{}` from any package -- every field is unexported
// -- so the zero value is constructible and must be REFUSED rather than
// dereferenced. A nil `topic.reveal` panics inside the alert path, which is the
// one code path whose job is to survive everything else failing.
func (s *NTFYSender) valid() bool {
	return s != nil && s.topic.Valid() && s.base != "" && s.http != nil
}

// errNoSender is fixed text. It names nothing.
var errNoSender = errors.New("this ntfy sender was not built by " +
	"NewNTFYSender: it addresses no channel, and delivering through it would " +
	"report success while telling nobody")

// Send posts one message.
//
// A non-2xx response and a transport error are BOTH failures, and neither marks
// anything delivered. §13's rows stay pending and are retried: an alert the
// operator has not seen is an alert that has not been delivered, whatever the
// process believed at the time.
func (s *NTFYSender) Send(ctx context.Context, m Message) error {
	if !s.valid() {
		return errNoSender
	}
	if m.Body == "" {
		return errors.New("refusing to send an empty notification body")
	}
	topic := s.topic.reveal()

	// The topic is a bearer credential. It addresses the channel and it is not
	// content: a title or body containing it puts the credential into every
	// notification history, every screenshot and every forwarded alert. This
	// check is on the CALLER's message; `M-P-TOPIC` appends the topic after it,
	// which is what `TestTopicNeverLeavesDestinationURL` inspects the delivered
	// request for.
	if strings.Contains(m.Title, topic) || strings.Contains(m.Body, topic) {
		return errors.New("refusing to send a notification whose title or " +
			"body contains the ntfy topic: the topic is the credential for " +
			"the channel, and its only permitted occurrence is the " +
			"destination URL")
	}

	dest := s.base + "/" + url.PathEscape(topic)
	body := m.Body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest,
		strings.NewReader(body))
	if err != nil {
		return transportError("building the ntfy request", err)
	}
	if m.Title != "" {
		req.Header.Set("Title", m.Title)
	}
	priority := m.Priority
	if priority == "" {
		priority = PriorityDefault
	}
	req.Header.Set("Priority", priority)

	resp, err := s.http.Do(req)
	if err != nil {
		// NEVER wrapped. The error carries the destination URL, and the
		// destination URL is the credential.
		return transportError("ntfy delivery", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy answered %s; the notification was not "+
			"delivered and its rows stay pending", resp.Status)
	}
	return nil
}

// confidence: high
