package tasks

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport answers every request without touching the network and
// remembers what it was asked to send. The alert senders post through
// http.DefaultClient, so this is the only seam that keeps a mistakenly enabled
// alert from leaving the test machine.
type recordingTransport struct {
	mu       sync.Mutex
	requests []recordedRequest

	// status is the code every reply carries; err, when set, fails the request
	// instead, as an unreachable endpoint would.
	status int
	err    error
}

type recordedRequest struct {
	url  string
	body string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = string(raw)
	}

	r.mu.Lock()
	r.requests = append(r.requests, recordedRequest{url: req.URL.String(), body: body})
	status, failure := r.status, r.err
	r.mu.Unlock()

	if failure != nil {
		return nil, failure
	}

	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (r *recordingTransport) recorded() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func interceptHTTP(t *testing.T) *recordingTransport {
	t.Helper()

	transport := &recordingTransport{status: http.StatusOK}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })

	return transport
}

// alertRunner is a task that finished badly, with everything the senders read
// already in place so none of them reaches the database.
func alertRunner(alert bool) *TaskRunner {
	version := "1.0.0"

	return &TaskRunner{
		Task: db.Task{
			ID:      42,
			Status:  task_logger.TaskFailStatus,
			Message: "boom",
			Version: &version,
		},
		Template: db.Template{ID: 7, ProjectID: 1, Name: "Deploy"},
		alert:    alert,
		pool:     &TaskPool{logger: make(chan logRecord, 64)},
	}
}

// taskLog returns what the task recorded, which is the only place a failed
// alert is reported: sending is best-effort and never fails the task.
func taskLog(runner *TaskRunner) string {
	lines := make([]string, 0)

	for {
		select {
		case record := <-runner.pool.logger:
			lines = append(lines, record.output)
		default:
			return strings.Join(lines, "\n")
		}
	}
}

type alertSender struct {
	name    string
	enable  func(*util.ConfigType)
	send    func(*TaskRunner)
	urlPart string
}

func alertSenders() []alertSender {
	return []alertSender{
		{
			"telegram",
			func(c *util.ConfigType) { c.TelegramAlert, c.TelegramChat = true, "chat-1" },
			(*TaskRunner).sendTelegramAlert,
			"api.telegram.org",
		},
		{
			"slack",
			func(c *util.ConfigType) { c.SlackAlert, c.SlackUrl = true, "https://hooks.invalid/slack" },
			(*TaskRunner).sendSlackAlert,
			"/slack",
		},
		{
			"rocketchat",
			func(c *util.ConfigType) { c.RocketChatAlert, c.RocketChatUrl = true, "https://hooks.invalid/rocket" },
			(*TaskRunner).sendRocketChatAlert,
			"/rocket",
		},
		{
			"microsoft teams",
			func(c *util.ConfigType) {
				c.MicrosoftTeamsAlert, c.MicrosoftTeamsUrl = true, "https://hooks.invalid/teams"
			},
			(*TaskRunner).sendMicrosoftTeamsAlert,
			"/teams",
		},
		{
			"dingtalk",
			func(c *util.ConfigType) { c.DingTalkAlert, c.DingTalkUrl = true, "https://hooks.invalid/dingtalk" },
			(*TaskRunner).sendDingTalkAlert,
			"/dingtalk",
		},
		{
			"gotify",
			func(c *util.ConfigType) { c.GotifyAlert, c.GotifyUrl = true, "https://hooks.invalid/gotify" },
			(*TaskRunner).sendGotifyAlert,
			"/gotify",
		},
	}
}

// enabledConfig replaces the global config with one where only this sender is
// switched on.
func enabledConfig(t *testing.T, sender alertSender) {
	t.Helper()

	setupExecutorConfig(t)
	util.Config = &util.ConfigType{}
	sender.enable(util.Config)
}

// Each alert channel has an on/off switch in the server config, and the task
// itself carries whether alerting was asked for at all. Neither is decoration:
// with either one off, nothing may leave the process.
func TestAlertsAreNotSentWhenSwitchedOff(t *testing.T) {
	for _, sender := range alertSenders() {
		t.Run(sender.name, func(t *testing.T) {
			t.Run("sent when the channel and the task both ask for it", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)

				runner := alertRunner(true)
				sender.send(runner)

				recorded := transport.recorded()
				require.Len(t, recorded, 1)
				assert.Contains(t, recorded[0].url, sender.urlPart)
				assert.Contains(t, taskLog(runner), "Sent successfully")
			})

			t.Run("silent when the channel is off", func(t *testing.T) {
				setupExecutorConfig(t)
				util.Config = &util.ConfigType{}
				transport := interceptHTTP(t)

				sender.send(alertRunner(true))

				assert.Empty(t, transport.recorded())
			})

			t.Run("silent when the task did not ask for alerts", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)

				sender.send(alertRunner(false))

				assert.Empty(t, transport.recorded())
			})

			// A template that only wants to hear about failures is the one case
			// where an enabled channel still sends nothing.
			t.Run("silent for a success the template suppresses", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)

				runner := alertRunner(true)
				runner.Task.Status = task_logger.TaskSuccessStatus
				runner.Template.SuppressSuccessAlerts = true

				sender.send(runner)

				assert.Empty(t, transport.recorded())
			})

			t.Run("a failure is still reported when successes are suppressed", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)

				runner := alertRunner(true)
				runner.Template.SuppressSuccessAlerts = true

				sender.send(runner)

				assert.Len(t, transport.recorded(), 1)
			})
		})
	}
}

// Delivery is best-effort: the task must not fail because a chat service did,
// so the task log is the only record that anything went wrong.
func TestAlertDeliveryFailuresAreLogged(t *testing.T) {
	for _, sender := range alertSenders() {
		t.Run(sender.name, func(t *testing.T) {
			t.Run("rejected by the service", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)
				transport.status = http.StatusInternalServerError

				runner := alertRunner(true)
				sender.send(runner)

				logged := taskLog(runner)
				assert.Contains(t, logged, "Response code: 500")
				assert.NotContains(t, logged, "Sent successfully")
			})

			t.Run("unreachable service", func(t *testing.T) {
				enabledConfig(t, sender)
				transport := interceptHTTP(t)
				transport.err = errors.New("connection refused")

				runner := alertRunner(true)
				sender.send(runner)

				logged := taskLog(runner)
				assert.Contains(t, logged, "connection refused")
				assert.NotContains(t, logged, "Sent successfully")
			})
		})
	}
}

// The payload is what the recipient actually reads, and the version is the part
// that says which release the failed task was running.
func TestAlertPayloadCarriesTheTaskVersion(t *testing.T) {
	sender := alertSenders()[1] // slack, whose template is plain JSON
	enabledConfig(t, sender)
	transport := interceptHTTP(t)

	runner := alertRunner(true)
	sender.send(runner)

	recorded := transport.recorded()
	require.Len(t, recorded, 1)
	assert.Contains(t, recorded[0].body, "1.0.0")

	// A task with no version of its own leaves the field empty rather than
	// inventing one.
	transport = interceptHTTP(t)
	runner = alertRunner(true)
	runner.Task.Version = nil
	sender.send(runner)

	recorded = transport.recorded()
	require.Len(t, recorded, 1)
	assert.NotContains(t, recorded[0].body, "1.0.0")
}

// The chat a task reports to may be overridden per task; an empty override is
// not a choice and must leave the server-wide chat in place.
func TestTelegramAlertChatOverride(t *testing.T) {
	sender := alertSenders()[0]

	chat := func(override *string) string {
		enabledConfig(t, sender)
		transport := interceptHTTP(t)

		runner := alertRunner(true)
		runner.alertChat = override
		sender.send(runner)

		recorded := transport.recorded()
		require.Len(t, recorded, 1)
		return recorded[0].body
	}

	empty := ""
	other := "chat-2"

	assert.Contains(t, chat(nil), "chat-1")
	assert.Contains(t, chat(&empty), "chat-1")
	assert.Contains(t, chat(&other), "chat-2")
}

// Without a chat there is nowhere to send: the alert is dropped before the
// request is built.
func TestTelegramAlertWithoutChat(t *testing.T) {
	setupExecutorConfig(t)
	util.Config = &util.ConfigType{TelegramAlert: true}
	transport := interceptHTTP(t)

	alertRunner(true).sendTelegramAlert()

	assert.Empty(t, transport.recorded())
}
