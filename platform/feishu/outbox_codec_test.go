package feishu

import (
	"errors"
	"fmt"
	"testing"
)

func TestReplyContextCodec_RoundTrip(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	orig := replyContext{
		messageID:       "om_1",
		chatID:          "oc_1",
		sessionKey:      "feishu:oc_1:u_1",
		threadID:        "omt_1",
		replyInThread:   true,
		bootstrapThread: true,
	}
	b, err := p.EncodeReplyCtx(orig)
	if err != nil {
		t.Fatalf("EncodeReplyCtx: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("encoded context is empty")
	}
	dec, err := p.DecodeReplyCtx(b)
	if err != nil {
		t.Fatalf("DecodeReplyCtx: %v", err)
	}
	got, ok := dec.(replyContext)
	if !ok {
		t.Fatalf("decoded type = %T, want replyContext", dec)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, orig)
	}
}

func TestReplyContextCodec_RejectsWrongType(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	if _, err := p.EncodeReplyCtx("not-a-reply-context"); err == nil {
		t.Fatal("expected error encoding wrong type")
	}
	if _, err := p.DecodeReplyCtx([]byte("{not json")); err == nil {
		t.Fatal("expected error decoding invalid json")
	}
}

func TestPlatform_IsRetryableSendError(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", fmt.Errorf("dial tcp 220.181.175.91:443: connect: connection refused"), true},
		{"io timeout", errors.New("i/o timeout"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"no such host is permanent", errors.New("dial tcp: lookup x.invalid: no such host"), false},
		{"plain business error is permanent", errors.New("230002 bot not in chat"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.IsRetryableSendError(c.err); got != c.want {
				t.Fatalf("IsRetryableSendError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
