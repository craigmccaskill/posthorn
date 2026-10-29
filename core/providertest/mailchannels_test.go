package providertest

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/craigmccaskill/posthorn/transport"
)

type mailchannelsWire struct {
	Personalizations []struct {
		To []struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"to"`
	} `json:"personalizations"`
	From struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"from"`
	Subject string `json:"subject"`
	Content []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"content"`
}

// mailchannelsAddress joins the API's {email, name} object back into the
// RFC 5322 form the scenario asserts against.
func mailchannelsAddress(email, name string) string {
	if name == "" {
		return email
	}
	return name + " <" + email + ">"
}

var mailchannelsTUT = TransportUnderTest{
	Name:     "mailchannels",
	Response: `{"request_id":"req-1","results":[{"index":0,"message_id":"mc-1","status":"sent"}]}`,
	NewHermetic: func(baseURL string) transport.Transport {
		return transport.NewMailChannelsTransport("test-key", baseURL)
	},
	Decode: func(t *testing.T, req CapturedRequest) DeliveredMail {
		t.Helper()
		var w mailchannelsWire
		if err := json.Unmarshal(req.Body, &w); err != nil {
			t.Fatalf("mailchannels body not valid JSON: %v\n%s", err, req.Body)
		}
		if len(w.Personalizations) == 0 {
			t.Fatalf("no personalizations in mailchannels body: %s", req.Body)
		}
		to := make([]string, 0, len(w.Personalizations[0].To))
		for _, r := range w.Personalizations[0].To {
			to = append(to, mailchannelsAddress(r.Email, r.Name))
		}
		bodyText := ""
		for _, c := range w.Content {
			if c.Type == "text/plain" {
				bodyText = c.Value
				break
			}
		}
		return DeliveredMail{
			From:     mailchannelsAddress(w.From.Email, w.From.Name),
			To:       to,
			Subject:  w.Subject,
			BodyText: bodyText,
		}
	},
	SafetyCheck: AssertNoRawCRLF,
	CheckAuth: func(t *testing.T, req CapturedRequest) {
		t.Helper()
		if got := req.Header.Get("X-Api-Key"); got != "test-key" {
			t.Errorf("X-Api-Key = %q, want %q", got, "test-key")
		}
	},
}

func TestE2E_MailChannels_FormToWire(t *testing.T) {
	RunFormScenario(t, mailchannelsTUT, Scenario{
		Name:     "contact",
		Endpoint: contactEndpoint(),
		Form:     url.Values{"name": {"Alice"}, "message": {"Hello there"}},
		Want: DeliveredMail{
			From:     "Posthorn <noreply@example.com>",
			To:       []string{"ops@example.com"},
			Subject:  "Contact from Alice",
			BodyText: "Hello there",
		},
		InjectField: "name",
	})
}
