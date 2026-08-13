package notify

import "context"

// JournalEntry is everything a JournalFunc ever receives about a delivery
// attempt: the event this notification was for, the run it concerned, and
// the outcome. Nothing else — never the webhook URL (the secret), never
// the payload body (which could grow to carry more than the URL alone
// reveals). This is the whole of the economy-discord-graphs journal
// boundary: "outcomes journaled without ever recording the URL or the
// payload."
type JournalEntry struct {
	Event   string
	RunID   string
	Outcome PostResult
}

// JournalFunc records a delivery outcome. Notify calls it at most once per
// call, after the delivery attempt (or non-attempt) is fully resolved. A
// nil JournalFunc is valid — Notify simply does not journal.
type JournalFunc func(JournalEntry)

// Notify resolves the webhook URL, shapes payload, attempts one bounded
// POST, and journals the outcome — the four steps a caller (the notifier
// daemon, t14) would otherwise have to sequence itself from ResolveWebhook,
// BuildMessage, and Post.
//
// journal is never called for a Disabled outcome: matching devex's own
// notify() (`_webhook.py`), a webhook that was never configured is not a
// delivery attempt worth a journal row — it is the ordinary, silent,
// no-op path every run takes when nobody has opted into Discord updates.
// Posted and Failed are always journaled, exactly once each, with no URL
// and no payload in the entry.
func Notify(ctx context.Context, payload Payload, journal JournalFunc) PostResult {
	rawURL, enabled := ResolveWebhook()
	if !enabled {
		return Disabled
	}

	var result PostResult
	body, err := BuildMessage(rawURL, payload)
	if err != nil {
		result = Failed
	} else {
		result = Post(ctx, rawURL, body)
	}

	if journal != nil {
		journal(JournalEntry{Event: payload.Event, RunID: payload.RunID, Outcome: result})
	}
	return result
}
