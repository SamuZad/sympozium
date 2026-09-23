package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// slackConfig captures Slack-specific gating configuration injected by the
// controller. All fields are optional; defaults preserve the prior
// "everything triggers, no threading" behaviour.
type slackConfig struct {
	threading        bool
	threadStickiness bool
	allowedTriggers  map[string]bool // "mention" | "dm" | "channel" — empty means allow all
	allowedSenders   map[string]bool // empty means allow all
	deniedSenders    map[string]bool
	allowedChats     map[string]bool // empty means allow all
}

// validTriggerKinds enumerates the trigger values the gating pipeline
// understands. Anything else in SLACK_ALLOWED_TRIGGERS is a no-op (and
// therefore silently blocks that channel unless other kinds are also
// listed), so loadSlackConfig logs a warning when unknown values appear.
var validTriggerKinds = map[string]bool{
	string(kindDM):      true,
	string(kindMention): true,
	string(kindChannel): true,
}

func loadSlackConfig(log logr.Logger) *slackConfig {
	cfg := &slackConfig{
		threading:        os.Getenv("SLACK_THREADING") == "true",
		threadStickiness: os.Getenv("SLACK_THREAD_STICKINESS") == "true",
		allowedTriggers:  csvToSet(os.Getenv("SLACK_ALLOWED_TRIGGERS")),
		allowedSenders:   csvToSet(os.Getenv("SLACK_ALLOWED_SENDERS")),
		deniedSenders:    csvToSet(os.Getenv("SLACK_DENIED_SENDERS")),
		allowedChats:     csvToSet(os.Getenv("SLACK_ALLOWED_CHATS")),
	}
	for v := range cfg.allowedTriggers {
		if !validTriggerKinds[v] {
			log.Info("WARNING: SLACK_ALLOWED_TRIGGERS contains unknown value; it will never match",
				"value", v, "validValues", []string{string(kindDM), string(kindMention), string(kindChannel)})
		}
	}
	return cfg
}

func csvToSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

// triggerKind classifies an inbound Slack message.
type triggerKind string

const (
	kindDM       triggerKind = "dm"
	kindMention  triggerKind = "mention"
	kindChannel  triggerKind = "channel"
	kindReaction triggerKind = "reaction" // not a message; never gated
)

// classifyKind decides whether the message is a DM, an @-mention of the
// bot, or a generic channel/group message.
func classifyKind(channelType, text, botID string) triggerKind {
	if channelType == "im" {
		return kindDM
	}
	if botID != "" && strings.Contains(text, "<@"+botID+">") {
		return kindMention
	}
	return kindChannel
}

// addressedToOther reports whether a message that does not mention the
// bot opens by @-mentioning someone else — a user or a user group, e.g.
// "<@UMICHAEL> see above". Such messages are aimed at that person, not
// the bot. @here/@channel don't count. Only meaningful for kindChannel
// (bot not mentioned, not a DM); without a bot ID a leading mention
// might be the bot itself, so it never matches.
func addressedToOther(text, botID string) bool {
	if botID == "" {
		return false
	}
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<@") || strings.HasPrefix(t, "<!subteam^")
}

// accessAllowed enforces the user/chat allow-deny lists. Returns false when
// the message must be dropped before any state mutation.
func (c *slackConfig) accessAllowed(senderID, chatID string) bool {
	if len(c.allowedChats) > 0 && !c.allowedChats[chatID] {
		return false
	}
	if len(c.allowedSenders) > 0 && !c.allowedSenders[senderID] {
		return false
	}
	if c.deniedSenders[senderID] {
		return false
	}
	return true
}

// triggerAllowed returns true when the kind satisfies AllowedTriggers.
// Empty allowlist means all kinds pass.
func (c *slackConfig) triggerAllowed(k triggerKind) bool {
	if len(c.allowedTriggers) == 0 {
		return true
	}
	return c.allowedTriggers[string(k)]
}

// threadState tracks per-thread state under thread-stickiness mode.
//
// Sticky-thread semantics:
//   - The first sender to address the bot in a thread becomes the
//     thread's `owner`.
//   - Any message from anyone other than the owner — regardless of
//     access, trigger, or content — permanently marks the thread
//     `interrupted`, as does any message (owner's included) that opens
//     by addressing someone other than the bot.
//   - Once interrupted, every message (including from the owner) must
//     satisfy the trigger rules (e.g. @-mention) to be processed.
type threadState struct {
	owner       string // userID that opened the thread
	interrupted bool
	lastSeen    time.Time
}

// threadEngagement is a TTL-bounded map of (chat,thread) → state. Lives in
// memory in the Slack pod and is lost on restart or eviction. When a
// trigger arrives in a thread with no entry, the state is rebuilt from
// Slack history (see hydrate) so a lost interruption is not undone; the
// worst case is the owner having to @ the bot once after a restart.
type threadEngagement struct {
	mu      sync.Mutex
	entries map[string]*threadState
	ttl     time.Duration

	// history fetches a thread's messages for hydrate. Nil disables
	// hydration: unknown threads are treated as never seen.
	history threadHistoryFunc
}

// historyMsg is one message of a thread as returned by
// conversations.replies.
type historyMsg struct {
	User  string `json:"user"`
	Text  string `json:"text"`
	TS    string `json:"ts"`
	BotID string `json:"bot_id"`
}

// threadHistoryFunc returns up to limit messages of a thread in
// chronological order, parent first.
type threadHistoryFunc func(chatID, threadTS string, limit int) ([]historyMsg, error)

// maxHydrateMessages bounds how much history hydrate replays. A thread
// with more earlier messages than this is assumed interrupted.
const maxHydrateMessages = 1000

func newThreadEngagement(ttl time.Duration) *threadEngagement {
	return &threadEngagement{
		entries: map[string]*threadState{},
		ttl:     ttl,
	}
}

func threadKey(chatID, threadTS string) string {
	return chatID + "/" + threadTS
}

func (te *threadEngagement) get(chatID, threadTS string) *threadState {
	te.mu.Lock()
	defer te.mu.Unlock()
	st, ok := te.entries[threadKey(chatID, threadTS)]
	if !ok {
		return nil
	}
	return st
}

func (te *threadEngagement) update(chatID, threadTS string, fn func(*threadState)) {
	te.mu.Lock()
	defer te.mu.Unlock()
	k := threadKey(chatID, threadTS)
	st, ok := te.entries[k]
	if !ok {
		st = &threadState{}
		te.entries[k] = st
	}
	fn(st)
	st.lastSeen = time.Now()
}

// sweep periodically evicts stale entries until ctx is cancelled. Run in a
// goroutine. Cheaper than eviction on every get/update in busy channels.
func (te *threadEngagement) sweep(ctx context.Context, interval time.Duration) {
	if te.ttl <= 0 || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			te.evictStale()
		}
	}
}

// hydrate rebuilds the sticky state of a thread the pod has no record of
// by replaying its earlier messages (those before beforeTS) through
// evaluateInbound, so ownership and interruption come out exactly as if
// the pod had seen them live. The result is merged monotonically — an
// owner is never replaced and interrupted is never cleared — and cached
// even when empty so the thread is not fetched again. A returned error
// means nothing was cached.
func (te *threadEngagement) hydrate(cfg *slackConfig, botID, chatID, threadTS, beforeTS, channelType string) error {
	if te.history == nil {
		return nil
	}
	msgs, err := te.history(chatID, threadTS, maxHydrateMessages)
	if err != nil {
		return err
	}

	replay := newThreadEngagement(0)
	complete := false
	for _, m := range msgs {
		if !tsBefore(m.TS, beforeTS) {
			complete = true
			break
		}
		// Same filters the live handlers apply before gating.
		if m.User == "" || m.Text == "" || m.BotID != "" {
			continue
		}
		parentTS := threadTS
		if m.TS == threadTS {
			parentTS = "" // the parent arrived as a top-level message
		}
		evaluateInbound(cfg, replay, botID, m.User, chatID, parentTS, m.TS, channelType, m.Text)
	}
	replayed := replay.get(chatID, threadTS)
	tooLong := !complete && len(msgs) >= maxHydrateMessages

	te.update(chatID, threadTS, func(s *threadState) {
		if tooLong {
			s.interrupted = true
		}
		if replayed == nil {
			return
		}
		if s.owner == "" {
			s.owner = replayed.owner
		}
		s.interrupted = s.interrupted || replayed.interrupted
	})
	return nil
}

// tsBefore reports whether Slack timestamp a ("seconds.micros") is
// earlier than b.
func tsBefore(a, b string) bool {
	aSec, aFrac, _ := strings.Cut(a, ".")
	bSec, bFrac, _ := strings.Cut(b, ".")
	as, err1 := strconv.ParseInt(aSec, 10, 64)
	bs, err2 := strconv.ParseInt(bSec, 10, 64)
	af, err3 := strconv.ParseInt(aFrac, 10, 64)
	bf, err4 := strconv.ParseInt(bFrac, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return a < b
	}
	if as != bs {
		return as < bs
	}
	return af < bf
}

func (te *threadEngagement) evictStale() {
	te.mu.Lock()
	defer te.mu.Unlock()
	if te.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-te.ttl)
	for k, st := range te.entries {
		if st.lastSeen.Before(cutoff) {
			delete(te.entries, k)
		}
	}
}

// gateDecision is the per-message outcome from gating logic.
type gateDecision int

const (
	gateAllow gateDecision = iota
	gateDrop
)

// evaluateInbound runs the full gating pipeline for one inbound message.
// ts is the Slack ts of the message itself; for a top-level message
// under threading mode this becomes the anchor of the thread the bot
// will reply in, so we use it to record initial ownership.
//
// The second return value is a short, human-readable reason explaining
// the decision (e.g. "access:chat-not-allowed", "trigger:kind=channel",
// "sticky:thread-interrupted", "allow:owner-freeflow"). It is intended
// purely for operator logging.
func evaluateInbound(
	cfg *slackConfig,
	te *threadEngagement,
	botID, senderID, chatID, threadTS, ts, channelType, text string,
) (gateDecision, string) {
	kind := classifyKind(channelType, text, botID)
	toOther := kind == kindChannel && addressedToOther(text, botID)

	inThread := threadTS != ""
	sticky := cfg.threading && cfg.threadStickiness && inThread

	// A trigger in a thread the pod has no record of (restart, eviction,
	// or never seen) would claim ownership below. Rebuild the thread's
	// state from Slack first so an interrupted thread stays tag-only.
	// Only triggers pay for the lookup, and only when triggers are
	// restricted — otherwise stickiness changes nothing.
	if sticky && !toOther && te.get(chatID, threadTS) == nil && len(cfg.allowedTriggers) > 0 &&
		cfg.triggerAllowed(kind) && cfg.accessAllowed(senderID, chatID) {
		if err := te.hydrate(cfg, botID, chatID, threadTS, ts, channelType); err != nil {
			// History unknown: serve this trigger but grant no ownership,
			// so a lost interruption cannot be undone. The next trigger
			// retries the lookup.
			return gateAllow, fmt.Sprintf("sticky: thread history unavailable, allowed without ownership: %v", err)
		}
	}

	// Sticky-thread interruption: any sender other than the thread's
	// owner permanently marks the thread interrupted, regardless of
	// access control, trigger, or content. So does anyone — the owner
	// included — opening a message by addressing someone else ("@michael
	// see above"): the thread is now a conversation between people. This
	// runs before access control so even a denied user's message latches
	// the interrupt.
	if sticky {
		st := te.get(chatID, threadTS)
		if toOther || (st != nil && st.owner != "" && st.owner != senderID) {
			te.update(chatID, threadTS, func(s *threadState) {
				s.interrupted = true
			})
		}
	}

	if !cfg.accessAllowed(senderID, chatID) {
		return gateDrop, fmt.Sprintf("access denied: sender=%s chat=%s", senderID, chatID)
	}

	// "@michael see above" is aimed at Michael, not the bot: never run,
	// not even as owner free-flow. In a sticky thread the latch above
	// has already made the rest of the thread tag-only.
	if toOther {
		return gateDrop, "addressed to another user"
	}

	// Sticky-thread evaluation.
	if sticky {
		st := te.get(chatID, threadTS)
		switch {
		case st != nil && (st.interrupted || (st.owner != "" && st.owner != senderID)):
			// Either a non-owner is speaking, or the thread has been
			// interrupted (possibly with no known owner, when rebuilt
			// from history). Both require a trigger every time; lax
			// free-flow never returns once interrupted.
			if !cfg.triggerAllowed(kind) {
				return gateDrop, fmt.Sprintf("sticky interrupted/non-owner, trigger not allowed: kind=%s", kind)
			}
			return gateAllow, "sticky: trigger satisfied"
		case st == nil || st.owner == "":
			// Unowned thread: claim ownership iff this sender passes
			// the trigger rules (e.g. opening @-mention).
			if !cfg.triggerAllowed(kind) {
				return gateDrop, fmt.Sprintf("trigger not allowed: kind=%s", kind)
			}
			te.update(chatID, threadTS, func(s *threadState) {
				if s.owner == "" {
					s.owner = senderID
				}
			})
			return gateAllow, "sticky: claimed thread ownership"
		default:
			// Owner free-flow.
			te.update(chatID, threadTS, func(s *threadState) {})
			return gateAllow, "sticky: owner free-flow"
		}
	}

	// Stateless evaluation.
	if !cfg.triggerAllowed(kind) {
		return gateDrop, fmt.Sprintf("trigger not allowed: kind=%s", kind)
	}

	// When threading+stickiness is on and this top-level message will
	// spawn a new thread, record this sender as the thread's owner so
	// their plain follow-ups inside the thread are recognised.
	if cfg.threading && cfg.threadStickiness {
		anchor := threadTS
		if anchor == "" {
			anchor = ts
		}
		if anchor != "" {
			te.update(chatID, anchor, func(s *threadState) {
				if s.owner == "" {
					s.owner = senderID
				}
			})
		}
	}
	return gateAllow, fmt.Sprintf("allow: kind=%s", kind)
}

// resolveBotUserID calls Slack auth.test to discover the bot's own user ID,
// which is needed for @-mention detection in messages.
func resolveBotUserID(ctx context.Context, client *http.Client, botToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/auth.test",
		strings.NewReader(url.Values{}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		OK     bool   `json:"ok"`
		UserID string `json:"user_id"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if !body.OK {
		return "", fmt.Errorf("auth.test: %s", body.Error)
	}
	return body.UserID, nil
}

// resolveBotUserIDWithRetry calls resolveBotUserID with exponential backoff.
// Returns the bot user ID on success or the last error after attempts is
// exhausted. Callers decide whether failure is fatal (e.g. when the gating
// config requires @-mention detection to be useful).
func resolveBotUserIDWithRetry(ctx context.Context, client *http.Client, botToken string, attempts int, baseDelay time.Duration) (string, error) {
	var lastErr error
	delay := baseDelay
	for i := 0; i < attempts; i++ {
		id, err := resolveBotUserID(ctx, client, botToken)
		if err == nil {
			return id, nil
		}
		lastErr = err
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return "", lastErr
}
