package main

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassifyKind(t *testing.T) {
	cases := []struct {
		name        string
		channelType string
		text        string
		botID       string
		want        triggerKind
	}{
		{"dm via channel_type=im", "im", "hello", "UBOT", kindDM},
		{"mention with bot id", "channel", "hey <@UBOT> ping", "UBOT", kindMention},
		{"mention without bot id falls through", "channel", "hey <@UBOT> ping", "", kindChannel},
		{"plain channel message", "channel", "noise", "UBOT", kindChannel},
		{"group message no mention", "group", "noise", "UBOT", kindChannel},
		{"empty bot id never matches mention", "channel", "<@UBOT>", "", kindChannel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyKind(tc.channelType, tc.text, tc.botID); got != tc.want {
				t.Fatalf("classifyKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAccessAllowed(t *testing.T) {
	cfg := &slackConfig{
		allowedChats:   csvToSet("C1,C2"),
		allowedSenders: csvToSet("U1,U2"),
		deniedSenders:  csvToSet("U3"),
	}
	cases := []struct {
		name         string
		sender, chat string
		want         bool
	}{
		{"chat allowed, sender allowed", "U1", "C1", true},
		{"chat not in allowlist", "U1", "C9", false},
		{"sender not in allowlist", "U9", "C1", false},
		{"sender denylist overrides allowlist absence", "U3", "C1", false},
		{"sender denylist overrides allowlist", "U3", "C2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.accessAllowed(tc.sender, tc.chat); got != tc.want {
				t.Fatalf("accessAllowed(%q,%q) = %v, want %v", tc.sender, tc.chat, got, tc.want)
			}
		})
	}

	t.Run("empty allowlists permit anyone not denied", func(t *testing.T) {
		empty := &slackConfig{deniedSenders: csvToSet("U9")}
		if !empty.accessAllowed("U1", "C1") {
			t.Fatal("expected allow when no allowlists configured")
		}
		if empty.accessAllowed("U9", "C1") {
			t.Fatal("expected deny for explicitly denied sender")
		}
	})
}

func TestTriggerAllowed(t *testing.T) {
	t.Run("empty list allows everything", func(t *testing.T) {
		cfg := &slackConfig{}
		for _, k := range []triggerKind{kindDM, kindMention, kindChannel} {
			if !cfg.triggerAllowed(k) {
				t.Fatalf("expected %q to be allowed under empty list", k)
			}
		}
	})
	t.Run("only listed kinds allowed", func(t *testing.T) {
		cfg := &slackConfig{allowedTriggers: csvToSet("mention,dm")}
		if !cfg.triggerAllowed(kindMention) || !cfg.triggerAllowed(kindDM) {
			t.Fatal("mention and dm should be allowed")
		}
		if cfg.triggerAllowed(kindChannel) {
			t.Fatal("channel should be denied")
		}
	})
}

// --- evaluateInbound: stateless paths -----------------------------------------------

func TestEvaluateInbound_Stateless(t *testing.T) {
	te := newThreadEngagement(time.Hour)

	t.Run("access denial drops without state mutation", func(t *testing.T) {
		cfg := &slackConfig{deniedSenders: csvToSet("U3")}
		got, _ := evaluateInbound(cfg, te, "UBOT", "U3", "C1", "", "TS001", "channel", "hey <@UBOT>")
		if got != gateDrop {
			t.Fatalf("decision = %v, want gateDrop", got)
		}
		if st := te.get("C1", ""); st != nil {
			t.Fatalf("expected no state for denied user, got %+v", st)
		}
	})

	t.Run("kind not in allowlist drops top-level message", func(t *testing.T) {
		cfg := &slackConfig{allowedTriggers: csvToSet("mention")}
		got, _ := evaluateInbound(cfg, te, "UBOT", "U1", "C1", "", "TS001", "channel", "no mention here")
		if got != gateDrop {
			t.Fatalf("decision = %v, want gateDrop", got)
		}
	})

	t.Run("kind in allowlist allows top-level message", func(t *testing.T) {
		cfg := &slackConfig{allowedTriggers: csvToSet("mention")}
		got, _ := evaluateInbound(cfg, te, "UBOT", "U1", "C1", "", "TS001", "channel", "hey <@UBOT>")
		if got != gateAllow {
			t.Fatalf("decision = %v, want gateAllow", got)
		}
	})

	t.Run("dm bypasses allowlist when dm listed", func(t *testing.T) {
		cfg := &slackConfig{allowedTriggers: csvToSet("dm")}
		got, _ := evaluateInbound(cfg, te, "UBOT", "U1", "D1", "", "TS001", "im", "hello")
		if got != gateAllow {
			t.Fatalf("decision = %v, want gateAllow", got)
		}
	})
}

// --- evaluateInbound: thread stickiness state machine -------------------------------

func TestEvaluateInbound_StickyThread_HappyPath(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.0001"
	)

	// Alice opens the thread by mentioning the bot.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "hi <@UBOT>"); got != gateAllow {
		t.Fatalf("opening mention dropped: %v", got)
	}
	// Alice replies in-thread without mention — sticky should allow.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "follow-up"); got != gateAllow {
		t.Fatalf("sticky reply dropped: %v", got)
	}
	st := te.get(chat, thread)
	if st == nil || st.owner != alice || st.interrupted {
		t.Fatalf("unexpected state: %+v", st)
	}
}

func TestEvaluateInbound_StickyThread_StrangerInterrupts(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		bob    = "UBOB"
		chat   = "C1"
		thread = "1700000000.0001"
	)

	// Alice engages.
	_, _ = evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "hi <@UBOT>")

	// Bob (stranger) speaks in-thread without mention → drop, mark interrupted.
	if got, _ := evaluateInbound(cfg, te, botID, bob, chat, thread, "TS001", "channel", "lurking"); got != gateDrop {
		t.Fatalf("stranger plain text should be dropped, got %v", got)
	}
	st := te.get(chat, thread)
	if !st.interrupted {
		t.Fatal("expected interrupted=true after stranger spoke")
	}
	if st.owner == bob {
		t.Fatal("stranger must not become the owner")
	}

	// Alice replies without re-mentioning → must be dropped (interrupted).
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "still here"); got != gateDrop {
		t.Fatalf("alice plain reply after interruption should drop, got %v", got)
	}
	if !te.get(chat, thread).interrupted {
		t.Fatal("alice plain reply must not clear interrupted")
	}

	// Alice re-mentions → allowed, but interrupted stays true: sticky
	// free-flow never resumes once a stranger has touched the thread.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "hey <@UBOT> still there?"); got != gateAllow {
		t.Fatalf("alice re-mention should allow, got %v", got)
	}
	if !te.get(chat, thread).interrupted {
		t.Fatal("interrupted must remain true for the rest of the thread's life")
	}
	// Alice's next plain reply is dropped — every message now needs a trigger.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "follow-up"); got != gateDrop {
		t.Fatalf("alice plain reply after stranger interrupt must drop, got %v", got)
	}
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "<@UBOT> follow-up"); got != gateAllow {
		t.Fatalf("alice re-mention should always be allowed, got %v", got)
	}
}

func TestEvaluateInbound_StickyThread_StrangerWithMentionEngages(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		bob    = "UBOB"
		chat   = "C1"
		thread = "1700000000.0001"
	)
	_, _ = evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "<@UBOT>")
	// Bob joins with a mention: allowed (trigger satisfied) and engaged,
	// but the thread is now permanently interrupted because a non-engaged
	// sender appeared.
	if got, _ := evaluateInbound(cfg, te, botID, bob, chat, thread, "TS001", "channel", "<@UBOT> me too"); got != gateAllow {
		t.Fatalf("stranger with mention should be allowed: %v", got)
	}
	st := te.get(chat, thread)
	if st.owner != alice {
		t.Fatalf("expected alice still to be owner, got %+v", st)
	}
	if !st.interrupted {
		t.Fatalf("expected thread to be interrupted after stranger joined, got %+v", st)
	}
	// Alice's plain reply now drops — interrupted never resumes.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "thanks"); got != gateDrop {
		t.Fatalf("alice plain reply after stranger join must drop, got %v", got)
	}
}

func TestEvaluateInbound_StickyThread_DeniedStrangerInterrupts(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
		deniedSenders:    csvToSet("UEVIL"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		evil   = "UEVIL"
		chat   = "C1"
		thread = "1700000000.0001"
	)
	_, _ = evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "<@UBOT>")

	// Denied user speaks: dropped at access gate, but the thread is
	// marked interrupted because a non-engaged sender appeared.
	if got, _ := evaluateInbound(cfg, te, botID, evil, chat, thread, "TS001", "channel", "<@UBOT> hi"); got != gateDrop {
		t.Fatalf("denied user must drop, got %v", got)
	}
	st := te.get(chat, thread)
	if !st.interrupted {
		t.Fatal("denied stranger must interrupt the thread")
	}
	if st.owner == evil {
		t.Fatal("access-denied user must never become owner")
	}
	// Alice's plain reply is now dropped because the thread is interrupted.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "ok"); got != gateDrop {
		t.Fatalf("alice plain reply must drop after stranger interruption, got %v", got)
	}
	// Alice re-mentions: allowed (trigger satisfied), but interrupted
	// stays true \u2014 lax mode never resumes.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "<@UBOT> still there?"); got != gateAllow {
		t.Fatalf("alice re-mention must re-engage, got %v", got)
	}
	if !te.get(chat, thread).interrupted {
		t.Fatal("interrupted must persist permanently after stranger interrupted")
	}
}

func TestEvaluateInbound_StickyDisabled_RequiresMentionEveryTime(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: false, // off
		allowedTriggers:  csvToSet("mention"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.0001"
	)
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "<@UBOT>"); got != gateAllow {
		t.Fatalf("opening mention dropped: %v", got)
	}
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "TS001", "channel", "follow-up"); got != gateDrop {
		t.Fatalf("without stickiness, plain follow-up must drop: %v", got)
	}
}

// TestEvaluateInbound_TopLevelMentionEngagesForThreadFollowUp covers the
// real-world flow: user @-mentions the bot in a top-level message; the
// bot replies in a new thread keyed on the inbound message's ts; the
// user then sends a plain follow-up inside that thread. With
// threadStickiness=true that follow-up MUST pass without re-mention.
func TestEvaluateInbound_TopLevelMentionEngagesForThreadFollowUp(t *testing.T) {
	cfg := &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
	}
	te := newThreadEngagement(time.Hour)
	const (
		botID = "UBOT"
		alice = "UALICE"
		chat  = "C1"
		ts    = "1700000000.0001"
	)

	// Top-level message: threadTS empty, ts is the message's own ts.
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, "", ts, "channel", "<@UBOT> what is k8s?"); got != gateAllow {
		t.Fatalf("top-level mention dropped: %v", got)
	}
	// Bot replies in thread anchored on `ts`. Ownership must have been
	// recorded against that anchor.
	st := te.get(chat, ts)
	if st == nil || st.owner != alice {
		t.Fatalf("expected alice to be owner on ts=%q after opening mention, got %+v", ts, st)
	}

	// Plain follow-up arrives with threadTS == ts (Slack semantics).
	if got, _ := evaluateInbound(cfg, te, botID, alice, chat, ts, ts+".reply", "channel", "tell me more"); got != gateAllow {
		t.Fatalf("in-thread follow-up after top-level mention should allow, got %v", got)
	}
}

func TestThreadEngagement_TTLEvicts(t *testing.T) {
	te := newThreadEngagement(10 * time.Millisecond)
	te.update("C1", "T1", func(s *threadState) {
		s.owner = "U1"
	})
	if st := te.get("C1", "T1"); st == nil {
		t.Fatal("entry should exist immediately")
	}
	time.Sleep(20 * time.Millisecond)
	te.evictStale()
	if st := te.get("C1", "T1"); st != nil {
		t.Fatalf("entry should have been evicted, got %+v", st)
	}
}

// --- sticky-thread hydration from Slack history ------------------------------------

// fakeHistory serves a fixed thread history and counts fetches.
type fakeHistory struct {
	msgs  []historyMsg
	err   error
	calls int
}

func (f *fakeHistory) fetch(_, _ string, limit int) ([]historyMsg, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.msgs) > limit {
		return f.msgs[:limit], nil
	}
	return f.msgs, nil
}

func stickyMentionCfg() *slackConfig {
	return &slackConfig{
		threading:        true,
		threadStickiness: true,
		allowedTriggers:  csvToSet("mention"),
	}
}

// TestHydrate_InterruptedThreadStaysTagOnlyAfterRestart is the reported
// bug: Alice opens a thread, Bob joins, the pod loses its state, and
// Alice's next @-mention must not hand her free-flow back.
func TestHydrate_InterruptedThreadStaysTagOnlyAfterRestart(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		bob    = "UBOB"
		chat   = "C1"
		thread = "1700000000.000100"
	)
	h := &fakeHistory{msgs: []historyMsg{
		{User: alice, Text: "<@UBOT> hi", TS: thread},
		{User: botID, Text: "hello!", TS: "1700000001.000100", BotID: "B1"},
		{User: bob, Text: "me too", TS: "1700000002.000100"},
		{User: alice, Text: "<@UBOT> again", TS: "1700000003.000100"}, // the current message
	}}
	te := newThreadEngagement(time.Hour) // fresh pod: no state
	te.history = h.fetch

	got, reason := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000003.000100", "channel", "<@UBOT> again")
	if got != gateAllow {
		t.Fatalf("re-mention dropped: %v (%s)", got, reason)
	}
	st := te.get(chat, thread)
	if st == nil || st.owner != alice || !st.interrupted {
		t.Fatalf("expected alice-owned interrupted thread after hydration, got %+v", st)
	}
	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000004.000100", "channel", "plain"); got != gateDrop {
		t.Fatalf("plain follow-up in interrupted thread must drop after restart, got %v", got)
	}
	if h.calls != 1 {
		t.Fatalf("history fetched %d times, want 1", h.calls)
	}
}

// TestHydrate_UninterruptedOwnerResumesFreeFlow: after a restart the owner
// of a solo thread @-mentions once and gets free-flow back.
func TestHydrate_UninterruptedOwnerResumesFreeFlow(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.000100"
	)
	h := &fakeHistory{msgs: []historyMsg{
		{User: alice, Text: "<@UBOT> hi", TS: thread},
		{User: botID, Text: "hello!", TS: "1700000001.000100", BotID: "B1"},
		{User: alice, Text: "plain", TS: "1700000002.000100"},
		{User: alice, Text: "<@UBOT> still there?", TS: "1700000003.000100"},
	}}
	te := newThreadEngagement(time.Hour)
	te.history = h.fetch

	// A plain message into an unknown thread is dropped without a lookup.
	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000002.000100", "channel", "plain"); got != gateDrop {
		t.Fatalf("plain message into unknown thread should drop, got %v", got)
	}
	if h.calls != 0 {
		t.Fatalf("non-trigger must not fetch history, got %d calls", h.calls)
	}

	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000003.000100", "channel", "<@UBOT> still there?"); got != gateAllow {
		t.Fatalf("owner re-mention dropped: %v", got)
	}
	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000004.000100", "channel", "plain again"); got != gateAllow {
		t.Fatalf("owner free-flow should resume after hydration, got %v", got)
	}
}

// TestHydrate_InThreadClaimThenInterrupt: ownership claimed by a mention
// inside someone else's thread is replayed the same way as live.
func TestHydrate_InThreadClaimThenInterrupt(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		bob    = "UBOB"
		carol  = "UCAROL"
		chat   = "C1"
		thread = "1700000000.000100"
	)
	h := &fakeHistory{msgs: []historyMsg{
		{User: carol, Text: "lunch?", TS: thread},                    // parent, not a trigger
		{User: alice, Text: "<@UBOT> help", TS: "1700000001.000100"}, // claims ownership
		{User: bob, Text: "hey", TS: "1700000002.000100"},            // interrupts
	}}
	te := newThreadEngagement(time.Hour)
	te.history = h.fetch

	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000003.000100", "channel", "<@UBOT> again"); got != gateAllow {
		t.Fatalf("re-mention dropped: %v", got)
	}
	st := te.get(chat, thread)
	if st == nil || st.owner != alice || !st.interrupted {
		t.Fatalf("expected alice-owned interrupted thread, got %+v", st)
	}
}

// TestHydrate_HistoryErrorAllowsWithoutOwnership: when Slack can't be
// reached the trigger is served, but nobody gains free-flow and nothing
// is cached, so the next trigger retries.
func TestHydrate_HistoryErrorAllowsWithoutOwnership(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.000100"
	)
	h := &fakeHistory{err: errors.New("ratelimited")}
	te := newThreadEngagement(time.Hour)
	te.history = h.fetch

	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000003.000100", "channel", "<@UBOT> hi"); got != gateAllow {
		t.Fatalf("trigger should be allowed when history fails, got %v", got)
	}
	if st := te.get(chat, thread); st != nil {
		t.Fatalf("history failure must not cache state, got %+v", st)
	}
	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000004.000100", "channel", "plain"); got != gateDrop {
		t.Fatalf("plain follow-up must drop without ownership, got %v", got)
	}

	h.err = nil
	h.msgs = []historyMsg{{User: alice, Text: "<@UBOT> hi", TS: thread}}
	evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000005.000100", "channel", "<@UBOT> retry")
	if h.calls != 2 {
		t.Fatalf("next trigger should retry the lookup, got %d calls", h.calls)
	}
}

// TestHydrate_TooLongThreadIsInterrupted: history beyond the replay cap is
// assumed interrupted rather than claimable.
func TestHydrate_TooLongThreadIsInterrupted(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.000000"
	)
	msgs := make([]historyMsg, 0, maxHydrateMessages+1)
	msgs = append(msgs, historyMsg{User: "UCAROL", Text: "parent", TS: thread})
	for i := 1; i <= maxHydrateMessages; i++ {
		msgs = append(msgs, historyMsg{User: "UCAROL", Text: "chatter", TS: "1700000000." + fmt.Sprintf("%06d", i)})
	}
	h := &fakeHistory{msgs: msgs}
	te := newThreadEngagement(time.Hour)
	te.history = h.fetch

	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000500.000000", "channel", "<@UBOT> hi"); got != gateAllow {
		t.Fatalf("trigger dropped: %v", got)
	}
	st := te.get(chat, thread)
	if st == nil || st.owner != "" || !st.interrupted {
		t.Fatalf("expected unowned interrupted thread, got %+v", st)
	}
	if got, _ := evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000501.000000", "channel", "plain"); got != gateDrop {
		t.Fatalf("plain message in too-long thread must drop, got %v", got)
	}
}

func TestTSBefore(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1700000000.000100", "1700000000.000200", true},
		{"1700000000.000200", "1700000000.000100", false},
		{"1700000000.000100", "1700000000.000100", false},
		{"999999999.999999", "1700000000.000000", true},
		{"1700000001.000000", "1700000000.999999", false},
	}
	for _, tc := range cases {
		if got := tsBefore(tc.a, tc.b); got != tc.want {
			t.Errorf("tsBefore(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- messages addressed to someone other than the bot ------------------------------

func TestAddressedToOther(t *testing.T) {
	cases := []struct {
		name, text, botID string
		want              bool
	}{
		{"leading user mention", "<@UMICHAEL> see above", "UBOT", true},
		{"leading mention after whitespace", "  <@UMICHAEL> see above", "UBOT", true},
		{"leading user group", "<!subteam^S123|@oncall> see above", "UBOT", true},
		{"mention mid-sentence", "can you check what <@UMICHAEL> said?", "UBOT", false},
		{"leading @here", "<!here> heads up", "UBOT", false},
		{"no mention", "see above", "UBOT", false},
		{"unknown bot id never matches", "<@UMICHAEL> see above", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := addressedToOther(tc.text, tc.botID); got != tc.want {
				t.Fatalf("addressedToOther(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestEvaluateInbound_AddressedToOther(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		bob    = "UBOB"
		chat   = "C1"
		thread = "1700000000.000100"
	)

	t.Run("owner addressing someone else makes the thread tag-only", func(t *testing.T) {
		te := newThreadEngagement(time.Hour)
		cfg := stickyMentionCfg()
		evaluateInbound(cfg, te, botID, alice, chat, "", thread, "channel", "<@UBOT> hi")
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "1700000001.000100", "channel", "still free-flow"); got != gateAllow {
			t.Fatalf("owner free-flow should work before addressing anyone, got %v", got)
		}
		if got, reason := evaluateInbound(cfg, te, botID, alice, chat, thread, "1700000002.000100", "channel", "<@UMICHAEL> look at this"); got != gateDrop {
			t.Fatalf("message to michael should drop, got %v (%s)", got, reason)
		}
		if !te.get(chat, thread).interrupted {
			t.Fatal("owner addressing someone else must interrupt the thread")
		}
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "1700000003.000100", "channel", "jump"); got != gateDrop {
			t.Fatalf("plain follow-up after addressing someone else must drop, got %v", got)
		}
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "1700000004.000100", "channel", "<@UMICHAEL> see above <@UBOT> please proceed"); got != gateAllow {
			t.Fatalf("message tagging the bot too should run, got %v", got)
		}
	})

	t.Run("mid-sentence mention keeps free-flow", func(t *testing.T) {
		te := newThreadEngagement(time.Hour)
		cfg := stickyMentionCfg()
		evaluateInbound(cfg, te, botID, alice, chat, "", thread, "channel", "<@UBOT> hi")
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, thread, "1700000001.000100", "channel", "what did <@UMICHAEL> mean?"); got != gateAllow {
			t.Fatalf("got %v, want allow", got)
		}
		if te.get(chat, thread).interrupted {
			t.Fatal("mid-sentence mention must not interrupt")
		}
	})

	t.Run("non-owner addressing someone else still interrupts", func(t *testing.T) {
		te := newThreadEngagement(time.Hour)
		cfg := stickyMentionCfg()
		evaluateInbound(cfg, te, botID, alice, chat, "", thread, "channel", "<@UBOT> hi")
		if got, _ := evaluateInbound(cfg, te, botID, bob, chat, thread, "1700000001.000100", "channel", "<@UALICE> I'll take it"); got != gateDrop {
			t.Fatalf("got %v, want drop", got)
		}
		if !te.get(chat, thread).interrupted {
			t.Fatal("bob's message must interrupt the thread")
		}
	})

	t.Run("channel trigger skips messages to someone else", func(t *testing.T) {
		te := newThreadEngagement(time.Hour)
		cfg := &slackConfig{} // everything triggers
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, "", thread, "channel", "<@UMICHAEL> lunch?"); got != gateDrop {
			t.Fatalf("got %v, want drop", got)
		}
		if got, _ := evaluateInbound(cfg, te, botID, alice, chat, "", thread, "channel", "lunch?"); got != gateAllow {
			t.Fatalf("got %v, want allow", got)
		}
	})

	t.Run("DMs are always for the bot", func(t *testing.T) {
		te := newThreadEngagement(time.Hour)
		cfg := &slackConfig{allowedTriggers: csvToSet("dm")}
		if got, _ := evaluateInbound(cfg, te, botID, alice, "D1", "", thread, "im", "<@UMICHAEL> said to ask you"); got != gateAllow {
			t.Fatalf("got %v, want allow", got)
		}
	})
}

// TestHydrate_OwnerAddressingOtherIsReplayed: an owner's "@michael ..." seen
// only in history still leaves the thread tag-only after a restart.
func TestHydrate_OwnerAddressingOtherIsReplayed(t *testing.T) {
	const (
		botID  = "UBOT"
		alice  = "UALICE"
		chat   = "C1"
		thread = "1700000000.000100"
	)
	h := &fakeHistory{msgs: []historyMsg{
		{User: alice, Text: "<@UBOT> hi", TS: thread},
		{User: alice, Text: "<@UMICHAEL> look at this", TS: "1700000001.000100"},
	}}
	te := newThreadEngagement(time.Hour)
	te.history = h.fetch

	evaluateInbound(stickyMentionCfg(), te, botID, alice, chat, thread, "1700000002.000100", "channel", "<@UBOT> jump")
	if st := te.get(chat, thread); st == nil || !st.interrupted {
		t.Fatalf("expected interrupted thread after replay, got %+v", st)
	}
}
