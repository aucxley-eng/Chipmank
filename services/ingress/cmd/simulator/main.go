// Command simulator stands in for Meta until business verification exists.
//
// It signs payloads with the same helper production verifies with, so the
// signature contract is exercised by construction rather than by two
// implementations agreeing by luck. Each scenario targets one documented
// behaviour of the receiver.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aucxley/chipmank/services/ingress/internal/metaevent"
	"github.com/aucxley/chipmank/services/ingress/internal/verify"
)

type scenario struct {
	name string
	desc string
	body func() []byte
}

var (
	appID     = flag.String("app-id", "dev-app-1", "provider app id, used in the URL path")
	appSecret = flag.String("app-secret", "", "app secret used to sign (required)")
	baseURL   = flag.String("url", "http://localhost:8080", "ingress base URL")
	which     = flag.String("scenario", "message", "message|status|error|malformed|empty|all")
	count     = flag.Int("count", 1, "number of deliveries to send")
	delay     = flag.Duration("delay", 0, "pause between deliveries, for observing retries")
	tamper    = flag.Bool("tamper", false, "sign correctly then alter the body, to prove tampering is rejected")
	fixed     = flag.Bool("fixed", false, "use stable ids and timestamps, so repeated sends are byte-identical retries")
)

// frozenTS is used with -fixed so repeated invocations produce the exact same
// bytes, which is what Meta's retry actually looks like.
var frozenTS = int64(1735689600)

func timestamp() string {
	if *fixed {
		return fmt.Sprint(frozenTS)
	}
	return fmt.Sprint(time.Now().Unix())
}

const (
	waba = "waba-dev-1"
	pn   = "pn-dev-1"
)

func inboundMessage(id, from, text string) []byte {
	env := metaevent.Envelope{
		Object: "whatsapp_business_account",
		Entry: []metaevent.Entry{{
			ID: waba,
			Changes: []metaevent.Change{{
				Field: "messages",
				Value: mustJSON(metaevent.Value{
					MessagingProduct: "whatsapp",
					Metadata: metaevent.Metadata{
						DisplayPhoneNumber: "15550001111",
						PhoneNumberID:      pn,
					},
					Contacts: []metaevent.Contact{{WaID: from}},
					Messages: []metaevent.Message{{
						From:      from,
						ID:        id,
						Timestamp: timestamp(),
						Type:      "text",
						Text:      &metaevent.TextMessage{Body: text},
					}},
				}),
			}},
		}},
	}
	return mustJSON(env)
}

func statusReceipt(id, state, recipient string) []byte {
	env := metaevent.Envelope{
		Object: "whatsapp_business_account",
		Entry: []metaevent.Entry{{
			ID: waba,
			Changes: []metaevent.Change{{
				Field: "messages",
				Value: mustJSON(metaevent.Value{
					MessagingProduct: "whatsapp",
					Metadata: metaevent.Metadata{
						DisplayPhoneNumber: "15550001111",
						PhoneNumberID:      pn,
					},
					Statuses: []metaevent.Status{{
						ID:          id,
						Status:      state,
						Timestamp:   timestamp(),
						RecipientID: recipient,
					}},
				}),
			}},
		}},
	}
	return mustJSON(env)
}

func providerError(code int, title string) []byte {
	env := metaevent.Envelope{
		Object: "whatsapp_business_account",
		Entry: []metaevent.Entry{{
			ID: waba,
			Changes: []metaevent.Change{{
				Field: "messages",
				Value: mustJSON(metaevent.Value{
					MessagingProduct: "whatsapp",
					Metadata:         metaevent.Metadata{PhoneNumberID: pn},
					Errors:           []metaevent.Error{{Code: code, Title: title, Message: title}},
				}),
			}},
		}},
	}
	return mustJSON(env)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func scenarios() []scenario {
	return []scenario{
		{"message", "inbound text message", func() []byte {
			id := "wamid.SIM" + fmt.Sprint(time.Now().UnixNano())
			if *fixed {
				id = "wamid.SIMFIXED1"
			}
			return inboundMessage(id, "15551230000", "hello from the simulator")
		}},
		{"status", "delivery status receipt", func() []byte {
			return statusReceipt("wamid.SIMOUT", "delivered", "15551230000")
		}},
		{"error", "provider error notification", func() []byte {
			return providerError(131026, "Message undeliverable")
		}},
		{"malformed", "authenticated body that is not JSON", func() []byte {
			return []byte("<html>gateway says hi</html>")
		}},
		{"empty", "empty body", func() []byte { return []byte{} }},
	}
}

func main() {
	flag.Parse()

	if *appSecret == "" {
		fmt.Fprintln(os.Stderr, "simulator: -app-secret is required")
		flag.Usage()
		os.Exit(2)
	}

	all := scenarios()
	var selected []scenario
	if *which == "all" {
		selected = all
	} else {
		for _, s := range all {
			if s.name == *which {
				selected = append(selected, s)
			}
		}
		if len(selected) == 0 {
			fmt.Fprintf(os.Stderr, "simulator: unknown scenario %q; known: %s\n",
				*which, strings.Join(names(all), ", "))
			os.Exit(2)
		}
	}

	url := fmt.Sprintf("%s/webhooks/meta/%s", strings.TrimRight(*baseURL, "/"), *appID)
	client := &http.Client{Timeout: 15 * time.Second}

	fmt.Printf("posting to %s\n\n", url)

	for i := range *count {
		for _, s := range selected {
			body := s.body()
			signature := verify.Sign([]byte(*appSecret), body)

			sendBody := body
			note := ""
			if *tamper && len(body) > 0 {
				// Sign the original, then put different bytes on the wire.
				// Appending a space keeps the JSON equivalent, so only the
				// raw-byte digest can catch this.
				sendBody = append(bytes.Clone(body), ' ')
				note = " [body tampered after signing]"
			}

			status, resp, err := post(client, url, sendBody, signature)
			fmt.Printf("scenario=%-9s status=%s%s\n", s.name, status, note)
			if err != nil {
				fmt.Printf("             error: %v\n", err)
			} else if strings.TrimSpace(resp) != "" {
				fmt.Printf("             %s\n", strings.TrimSpace(resp))
			}
		}
		if i+1 < *count {
			time.Sleep(*delay)
		}
	}
}

func post(client *http.Client, url string, body []byte, signature string) (string, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(verify.SignatureHeader, signature)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Sprint(resp.StatusCode), string(out), nil
}

func names(ss []scenario) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.name)
	}
	return out
}
