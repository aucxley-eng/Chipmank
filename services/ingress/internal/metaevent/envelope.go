// Package metaevent models the Meta Cloud API webhook envelope and derives the
// stable identity of a delivery.
//
// Both the ingress server and the local simulator depend on this package, so
// the payloads the harness sends are produced by the same types the server
// parses. A simulator that invented its own JSON would eventually drift from
// what Meta really sends, and the tests would keep passing.
package metaevent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Envelope is the top level of every Meta webhook body.
type Envelope struct {
	Object string  `json:"object"`
	Entry  []Entry `json:"entry"`
}

// Entry groups changes for one WhatsApp Business Account.
type Entry struct {
	ID      string   `json:"id"`
	Changes []Change `json:"changes"`
}

// Change carries a single field update. The field is "messages" for both
// inbound messages and delivery status receipts, which is why Value keeps them
// side by side rather than treating them as separate shapes.
type Change struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

// Value is the payload of a "messages" change.
type Value struct {
	MessagingProduct string    `json:"messaging_product"`
	Metadata         Metadata  `json:"metadata"`
	Contacts         []Contact `json:"contacts"`
	Messages         []Message `json:"messages"`
	Statuses         []Status  `json:"statuses"`
	Errors           []Error   `json:"errors"`
}

// Metadata identifies which of the tenant's phone numbers the event arrived on.
type Metadata struct {
	DisplayPhoneNumber string `json:"display_phone_number"`
	PhoneNumberID      string `json:"phone_number_id"`
}

type Contact struct {
	Profile struct {
		Name string `json:"name"`
	} `json:"profile"`
	WaID string `json:"wa_id"`
}

type Message struct {
	From        string          `json:"from"`
	ID          string          `json:"id"`
	Timestamp   string          `json:"timestamp"`
	Type        string          `json:"type"`
	Text        *TextMessage    `json:"text,omitempty"`
	Image       json.RawMessage `json:"image,omitempty"`
	Audio       json.RawMessage `json:"audio,omitempty"`
	Video       json.RawMessage `json:"video,omitempty"`
	Document    json.RawMessage `json:"document,omitempty"`
	Sticker     json.RawMessage `json:"sticker,omitempty"`
	Location    json.RawMessage `json:"location,omitempty"`
	Contacts    json.RawMessage `json:"contacts,omitempty"`
	Interactive json.RawMessage `json:"interactive,omitempty"`
	Button      json.RawMessage `json:"button,omitempty"`
	Reaction    json.RawMessage `json:"reaction,omitempty"`
	Errors      []Error         `json:"errors,omitempty"`
}

type TextMessage struct {
	Body string `json:"body"`
}

type Status struct {
	ID           string          `json:"id"`
	Status       string          `json:"status"`
	Timestamp    string          `json:"timestamp"`
	RecipientID  string          `json:"recipient_id"`
	Errors       []Error         `json:"errors,omitempty"`
	Conversation json.RawMessage `json:"conversation,omitempty"`
	Pricing      json.RawMessage `json:"pricing,omitempty"`
}

type Error struct {
	Code      int    `json:"code"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	ErrorData *struct {
		Details string `json:"details"`
	} `json:"error_data,omitempty"`
}

// Kind classifies a single item within a delivery.
type Kind string

const (
	KindInboundMessage Kind = "inbound_message"
	KindStatus         Kind = "status"
	KindProviderError  Kind = "provider_error"
	KindUnrecognised   Kind = "unrecognised"
)

// Item is one addressable event extracted from a delivery.
type Item struct {
	Kind Kind

	// WABAID is the owning WhatsApp Business Account.
	WABAID string
	// PhoneNumberID routes the event to a specific tenant channel account.
	PhoneNumberID string
	// ProviderMessageID is the wamid for messages and statuses.
	ProviderMessageID string
	// ConversationKey groups items from the same Meta conversation, used only
	// as an optimisation hint; v2 forbids relying on it for final routing.
	ConversationKey string
	// Raw is the untouched per-item JSON, kept so later stages can re-parse
	// fields this struct does not model yet.
	Raw json.RawMessage
}

// Parse extracts every addressable item from an authenticated body.
//
// A body that is valid JSON but carries no recognisable items yields zero
// items and no error. That is deliberate: the event is still durably stored as
// a rejection so it is never retried forever and never silently lost.
func Parse(raw []byte) (Envelope, []Item, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, nil, fmt.Errorf("metaevent: decode envelope: %w", err)
	}
	if !strings.EqualFold(env.Object, "whatsapp_business_account") {
		// Not fatal. Meta adds object types over time, and an unknown object
		// still gets stored and acknowledged so it is not retried endlessly.
		return env, nil, nil
	}

	var items []Item
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				items = append(items, Item{
					Kind: KindUnrecognised, WABAID: entry.ID, Raw: change.Value,
				})
				continue
			}

			var v Value
			if err := json.Unmarshal(change.Value, &v); err != nil {
				// Keep the unparseable change rather than dropping it.
				items = append(items, Item{
					Kind: KindUnrecognised, WABAID: entry.ID, Raw: change.Value,
				})
				continue
			}

			for _, m := range v.Messages {
				b, _ := json.Marshal(m)
				items = append(items, Item{
					Kind: KindInboundMessage, WABAID: entry.ID,
					PhoneNumberID: v.Metadata.PhoneNumberID, ProviderMessageID: m.ID,
					Raw: b,
				})
			}
			for _, s := range v.Statuses {
				b, _ := json.Marshal(s)
				key := s.Conversation
				items = append(items, Item{
					Kind: KindStatus, WABAID: entry.ID,
					PhoneNumberID: v.Metadata.PhoneNumberID, ProviderMessageID: s.ID,
					ConversationKey: string(key), Raw: b,
				})
			}
			if len(v.Messages) == 0 && len(v.Statuses) == 0 {
				for _, e := range v.Errors {
					b, _ := json.Marshal(e)
					items = append(items, Item{
						Kind: KindProviderError, WABAID: entry.ID,
						PhoneNumberID: v.Metadata.PhoneNumberID, Raw: b,
					})
				}
			}
		}
	}
	return env, items, nil
}

// DeliveryKey is the idempotency key for one webhook delivery.
//
// Meta retries a webhook by re-sending the identical body, so hashing the raw
// bytes together with the app id collapses retries onto the same row. This is
// the mechanism behind the "duplicate webhook" case in v2 section 39, and it
// works without trusting any field inside the unverified body.
func DeliveryKey(providerAppID string, raw []byte) string {
	h := sha256.New()
	// Length-prefixing the app id keeps the two inputs unambiguous.
	h.Write([]byte(providerAppID))
	h.Write([]byte{0})
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// ItemFingerprint is the stable identity of one addressable item.
//
// v2 section 5 is explicit that item identity must be derived from provider
// identifiers and event-specific stable fields, never from arrival time,
// otherwise every redelivery looks like a new item and dedupe silently stops
// working. The raw item JSON is folded in so that two distinct events which
// happen to share a wamid cannot collide.
func ItemFingerprint(providerAppID string, it Item) string {
	h := sha256.New()
	for _, field := range []string{
		providerAppID,
		string(it.Kind),
		it.PhoneNumberID,
		it.ProviderMessageID,
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	h.Write(it.Raw)
	return hex.EncodeToString(h.Sum(nil))
}

// UnparsedFingerprint identifies an authenticated delivery that yielded no
// addressable items. It still needs an identity, or the row could not be
// deduplicated against a retry.
func UnparsedFingerprint(providerAppID, deliveryKey string) string {
	h := sha256.New()
	h.Write([]byte(providerAppID))
	h.Write([]byte{0})
	h.Write([]byte("unparsed"))
	h.Write([]byte{0})
	h.Write([]byte(deliveryKey))
	return hex.EncodeToString(h.Sum(nil))
}
