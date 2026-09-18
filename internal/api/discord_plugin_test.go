package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the REFERENCE Installed plugin: the Discord Event sink
// (.scratch/plugin-system issue 14).
//
// The module is built from the plugin's own source at test time — the sibling
// repository ../obelo-plugin-discord when it is checked out, the vendored copy
// under internal/plugins/discordtest/testdata otherwise — so a contract change
// that breaks the plugin an author is told to copy breaks THIS build.
//
// Everything asserted here is what an Admin can observe: a plugin uploaded on the
// Plugins screen, settings saved on it, a subscription on the Event Sinks screen,
// and messages arriving at a server standing in for Discord. Nothing reaches
// inside the server, and nothing here is a mock of the sandbox.

// --- the stand-in for Discord -------------------------------------------------

// discordMessage is the execute-webhook body, as Discord would parse it.
type discordMessage struct {
	Content         string `json:"content"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
		Roles []string `json:"roles"`
	} `json:"allowed_mentions"`
}

type discordPost struct {
	Path        string
	ContentType string
	UserAgent   string
	Raw         []byte
	Message     discordMessage
}

// discordStandIn is an httptest server playing Discord's webhook endpoint. It is
// the only thing in this file that is not the real system: the plugin, the
// sandbox, the allowlist and the settings are all the shipped ones.
type discordStandIn struct {
	srv *httptest.Server

	mu    sync.Mutex
	posts []discordPost
	bad   []string
}

// The webhook path the tests configure. The id half is public; the token half is
// the credential, which is why the whole URL is a `secret` settings field.
const (
	discordWebhookID    = "1234567890"
	discordWebhookToken = "a-webhook-token-that-is-a-credential"
)

func newDiscordStandIn(t *testing.T) *discordStandIn {
	t.Helper()
	d := &discordStandIn{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		post := discordPost{
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			UserAgent:   r.Header.Get("User-Agent"),
			Raw:         body,
		}
		d.mu.Lock()
		if err := json.Unmarshal(body, &post.Message); err != nil {
			d.bad = append(d.bad, string(body))
		}
		d.posts = append(d.posts, post)
		d.mu.Unlock()
		// Discord answers 204 to an execute-webhook with no ?wait=true.
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// webhookURL is what an operator pastes into the plugin's secret field.
func (d *discordStandIn) webhookURL() string {
	return d.srv.URL + "/api/webhooks/" + discordWebhookID + "/" + discordWebhookToken
}

func (d *discordStandIn) received() []discordPost {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]discordPost, len(d.posts))
	copy(out, d.posts)
	return out
}

// waitFor polls until at least n messages have arrived, or fails.
func (d *discordStandIn) waitFor(t *testing.T, n int) []discordPost {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := d.received()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("Discord received %d messages, want at least %d", len(got), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle waits long enough that a message which was going to arrive has arrived.
// Used only for "and nothing else was posted".
func (d *discordStandIn) settle() { time.Sleep(750 * time.Millisecond) }

// --- installing and configuring it, exactly as an Admin does ------------------

// installDiscordPlugin uploads the reference plugin through the Plugins screen —
// the same multipart POST the browser's form sends, with the manifest byte for
// byte as its author wrote it.
func installDiscordPlugin(t *testing.T, srv *testharness.Server, token string) {
	t.Helper()
	status, body := uploadPlugin(t, srv, token, discordtest.ManifestJSON(t), discordtest.Module(t))
	if status != http.StatusCreated {
		t.Fatalf("installing the Discord plugin: status = %d, want 201; body: %s", status, body)
	}
	if p := pluginNamed(t, readPlugins(t, srv, token), "discord"); !p.Enabled || p.DisabledByFailure {
		t.Fatalf("the freshly installed Discord plugin reads as %+v, want enabled and not stopped", p)
	}
}

// saveDiscordSettings fills the plugin's OWN settings — the ones its manifest
// declares — and fails unless the server took them.
func saveDiscordSettings(t *testing.T, srv *testharness.Server, token string, values map[string]any) {
	t.Helper()
	if status, body := saveDeclaredSettings(t, srv, token, "discord", values); status != http.StatusOK {
		t.Fatalf("saving the Discord plugin's settings: status = %d, want 200; body: %s", status, body)
	}
}

// subscribeDiscord turns the sink on through the Event Sinks screen: the same
// endpoint, and the same body, as the Webhook Built-in beside it.
//
// The Target URL is the stand-in's origin. In production it is
// `https://discord.com`, which is the one host this plugin's manifest allowlists
// — see the note in the plugin's README on why a Discord webhook URL cannot live
// in the fixed URL field.
func subscribeDiscord(t *testing.T, srv *testharness.Server, token, targetURL string, events ...string) {
	t.Helper()
	configureSink(t, srv, token, map[string]any{
		"slug":    "discord",
		"enabled": true,
		"url":     targetURL,
		"events":  events,
	})
}

// --- the acceptance criterion -------------------------------------------------

var scanMessage = regexp.MustCompile(`^📚 Scan of \*\*Movies\*\* finished: (\d+) titles, (\d+) files$`)

// TestTheDiscordPluginPostsAScanAndAPlay is the tracer bullet, whole: an Admin
// installs the built Discord plugin through the Plugins screen, pastes a webhook
// URL, subscribes it to scan.completed and playback.started, runs a scan and
// presses play — and two correctly formatted messages arrive at Discord.
func TestTheDiscordPluginPostsAScanAndAPlay(t *testing.T) {
	requireFixtures(t)
	discord := newDiscordStandIn(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)

	installDiscordPlugin(t, srv, token)
	saveDiscordSettings(t, srv, token, map[string]any{"webhook_url": discord.webhookURL()})
	subscribeDiscord(t, srv, token, discord.srv.URL, "scan.completed", "playback.started")

	// The scan.
	list := scanFixtureLibrary(t, srv, token)
	scanPost := discord.waitFor(t, 1)[0]

	if scanPost.Path != "/api/webhooks/"+discordWebhookID+"/"+discordWebhookToken {
		t.Fatalf("posted to %q, want the webhook URL the Admin pasted", scanPost.Path)
	}
	if scanPost.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json — Discord refuses anything else", scanPost.ContentType)
	}
	// The host writes the User-Agent and names the Plugin in it, so an operator
	// reading Discord's own logs can tell which code posted.
	if !strings.Contains(scanPost.UserAgent, "plugin discord") {
		t.Errorf("User-Agent = %q, want the host's own, naming the plugin", scanPost.UserAgent)
	}
	m := scanMessage.FindStringSubmatch(scanPost.Message.Content)
	if m == nil {
		t.Fatalf("scan message = %q, want the manifest's default template rendered against the event",
			scanPost.Message.Content)
	}
	if m[1] == "0" || m[2] == "0" {
		t.Errorf("scan message = %q, but the fixture library is not empty", scanPost.Message.Content)
	}
	// Nobody is pinged unless the Admin asked, whatever a Title happens to be
	// called. An ABSENT allowed_mentions would let Discord resolve every @.
	if scanPost.Message.AllowedMentions.Parse == nil || len(scanPost.Message.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want a present but empty array",
			scanPost.Message.AllowedMentions.Parse)
	}
	if len(scanPost.Message.AllowedMentions.Roles) != 0 {
		t.Errorf("allowed_mentions.roles = %v on a plugin with no role configured",
			scanPost.Message.AllowedMentions.Roles)
	}

	// The play.
	titleID := findTitle(t, list, "Dune")
	var dec decisionResp
	if status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback",
		token, mp4Profile(), &dec); status != http.StatusOK {
		t.Fatalf("negotiate = %d; body: %s", status, raw)
	}

	posts := discord.waitFor(t, 2)
	discord.settle()
	playPost := posts[1]
	if want := "▶️ **Dune** — started by brandon"; playPost.Message.Content != want {
		t.Fatalf("play message = %q, want %q", playPost.Message.Content, want)
	}

	// Two events subscribed, two messages. playback.stopped and the rest were not
	// subscribed and must not arrive — the HOST filters, so the plugin never had
	// to check.
	if got := discord.received(); len(got) != 2 {
		t.Fatalf("Discord received %d messages for one scan and one play, want 2: %+v", len(got), got)
	}
	// Nothing about the filesystem or the catalog crosses this boundary.
	for _, p := range discord.received() {
		if strings.Contains(string(p.Raw), srv.DataDir) || strings.Contains(string(p.Raw), "rootFolders") {
			t.Fatalf("a message carried filesystem or catalog detail: %s", p.Raw)
		}
	}

	// And the counters an Admin reads while developing agree.
	view := waitForSink(t, srv, token, "discord", func(s installedSinkResp) bool {
		return s.Counters.Delivered >= 2
	})
	if view.Counters.Failed != 0 || view.Disabled || view.LastError != "" {
		t.Fatalf("after two clean deliveries the sink reads %+v, want no failures", view)
	}
	if !view.Installed || view.Version != "1.0.0" {
		t.Fatalf("the sink reads %+v, want it marked Installed at the manifest's version", view)
	}
}

// TestTheDiscordPluginMentionsARoleOnlyWhenTheAdminAsks: the toggle, and the
// promise behind it. With it off nothing this plugin posts can ping anybody; with
// it on, exactly one role can, and it is named in allowed_mentions as well as in
// the text — because the text alone would be honoured for @everyone and ignored
// for a role.
func TestTheDiscordPluginMentionsARoleOnlyWhenTheAdminAsks(t *testing.T) {
	discord := newDiscordStandIn(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	installDiscordPlugin(t, srv, token)
	saveDiscordSettings(t, srv, token, map[string]any{
		"webhook_url":  discord.webhookURL(),
		"mention_role": true,
		"role_id":      "555000111",
	})
	subscribeDiscord(t, srv, token, discord.srv.URL, "scan.completed")

	scanLib(t, srv, token, libID, "")
	post := discord.waitFor(t, 1)[0]

	if !strings.HasPrefix(post.Message.Content, "<@&555000111> 📚 Scan of ") {
		t.Fatalf("message = %q, want the role ping in front of the rendered template", post.Message.Content)
	}
	if got := post.Message.AllowedMentions.Roles; len(got) != 1 || got[0] != "555000111" {
		t.Fatalf("allowed_mentions.roles = %v, want exactly the configured role — Discord ignores a role ping that is not listed", got)
	}
	if len(post.Message.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want empty even with a role configured", post.Message.AllowedMentions.Parse)
	}

	// Turning it off again takes effect on the NEXT delivery, with no restart and
	// no rebuild: the declared settings are stamped onto every call.
	saveDiscordSettings(t, srv, token, map[string]any{"mention_role": false})
	scanLib(t, srv, token, libID, "")
	next := discord.waitFor(t, 2)[1]
	if strings.HasPrefix(next.Message.Content, "<@&") {
		t.Fatalf("message = %q, want no ping after the Admin turned the toggle off", next.Message.Content)
	}
	if len(next.Message.AllowedMentions.Roles) != 0 {
		t.Errorf("allowed_mentions.roles = %v after the toggle was turned off", next.Message.AllowedMentions.Roles)
	}
}

// TestTheDiscordPluginNamesALinkedServerAndNeverAPerson is ADR-0054 §3 as far as
// it reaches: all the way out to somebody's Discord channel.
//
// Somebody in another household presses play on a Title of ours. The sharing
// Admin's Discord gets a message — that is their load, their content, their
// Playback ceiling being spent — and it names the LINKED SERVER by the label they
// typed for it. The person on the far sofa is not named, because their name never
// crossed the wire and this plugin has nothing it could print.
func TestTheDiscordPluginNamesALinkedServerAndNeverAPerson(t *testing.T) {
	discord := newDiscordStandIn(t)
	f := linkForRelay(t)

	// The far household's own viewer, whose name must never reach Discord.
	f.home.CreateUser(f.homeAdmin, "faraway-viewer", "viewerpass123", "member")
	grantLibraries(t, f.home, f.homeAdmin, userIDByName(t, f.home, f.homeAdmin, "faraway-viewer"), f.mirrorLib)
	viewerTok := f.home.LoginAs("faraway-viewer", "viewerpass123")

	// The plugin is the SHARER's: this is the sharing Admin's automation.
	installDiscordPlugin(t, f.sharer, f.sharerAdmin)
	saveDiscordSettings(t, f.sharer, f.sharerAdmin, map[string]any{"webhook_url": discord.webhookURL()})
	subscribeDiscord(t, f.sharer, f.sharerAdmin, discord.srv.URL, "playback.started")

	titleID := f.mirroredTitle(t, "Dune")
	if status, _, raw := f.play(t, viewerTok, titleID, mp4Profile()); status != http.StatusOK {
		t.Fatalf("relayed play = %d; body: %s", status, raw)
	}

	post := discord.waitFor(t, 1)[0]
	if want := "▶️ **Dune** — started by The other household (a linked server)"; post.Message.Content != want {
		t.Fatalf("message = %q, want %q", post.Message.Content, want)
	}
	// The whole-body assertion: nothing the far household calls anything by
	// appears anywhere in anything this plugin sent.
	discord.settle()
	for _, p := range discord.received() {
		for _, name := range []string{"faraway-viewer", "faraway-viewer-device", "faraway-viewer-client"} {
			if strings.Contains(string(p.Raw), name) {
				t.Fatalf("a name from the other household reached Discord (%q): %s", name, p.Raw)
			}
		}
	}
}

// TestTheDiscordPluginMayNotPostAnywhereButDiscord is the allowlist, proved on
// the reference plugin.
//
// The manifest names one host. An Admin who pastes a webhook URL on some OTHER
// host — a phishing page dressed as Discord, a typo'd domain — gets a refusal
// from the HOST, not from the plugin: the request is never made, the attempt is
// audited, and after enough of them this server stops calling the plugin and says
// on the screen which host it reached for.
func TestTheDiscordPluginMayNotPostAnywhereButDiscord(t *testing.T) {
	discord := newDiscordStandIn(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	installDiscordPlugin(t, srv, token)
	// The Target URL is a host the operator chose, so it is one the host would
	// allow. The WEBHOOK URL is not, and it is the one the plugin fetches.
	saveDiscordSettings(t, srv, token, map[string]any{
		"webhook_url": "https://not-discord.example.test/api/webhooks/1/stolen",
	})
	subscribeDiscord(t, srv, token, discord.srv.URL, "scan.completed")

	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		scanLib(t, srv, token, libID, "")
	}

	view := waitForSink(t, srv, token, "discord", func(s installedSinkResp) bool { return s.Disabled })
	if !strings.Contains(view.LastError, "not-discord.example.test") {
		t.Fatalf("lastError = %q, want it to name the host the plugin reached for", view.LastError)
	}
	if view.Counters.Delivered != 0 {
		t.Fatalf("a plugin whose every fetch was refused reports %d deliveries", view.Counters.Delivered)
	}
	// Nothing was sent anywhere — not to the forbidden host, and not to the
	// operator's own Target URL either. The delivery never got that far.
	if got := discord.received(); len(got) != 0 {
		t.Fatalf("a plugin whose fetch was refused still posted %d messages: %+v", len(got), got)
	}
	// It is still ENABLED in settings: the Admin switched it on and this server
	// stopped it, which is exactly the state that needs explaining.
	if !view.Enabled {
		t.Fatalf("the Admin's enabled toggle was silently flipped: %+v", view)
	}
}

// TestTheDiscordPluginSaysNothingForAnEventWithNoTemplate: the manifest ships
// `template_library_changed` empty, because that event fires often and nobody
// wants it in a channel by default. A cleared template is an instruction, not a
// failure — so nothing is posted AND nothing is counted as failed.
func TestTheDiscordPluginSaysNothingForAnEventWithNoTemplate(t *testing.T) {
	discord := newDiscordStandIn(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	installDiscordPlugin(t, srv, token)
	saveDiscordSettings(t, srv, token, map[string]any{"webhook_url": discord.webhookURL()})
	subscribeDiscord(t, srv, token, discord.srv.URL, "library.changed", "scan.completed")

	scanLib(t, srv, token, libID, "")

	// The scan message arrives; the library.changed one that rides alongside it
	// does not.
	post := discord.waitFor(t, 1)[0]
	if !strings.HasPrefix(post.Message.Content, "📚 Scan of ") {
		t.Fatalf("message = %q, want the scan template", post.Message.Content)
	}
	discord.settle()
	if got := discord.received(); len(got) != 1 {
		t.Fatalf("Discord received %d messages, want only the scan one: %+v", len(got), got)
	}
	view := waitForSink(t, srv, token, "discord", func(s installedSinkResp) bool {
		return s.Counters.Delivered >= 1
	})
	if view.Counters.Failed != 0 || view.Disabled {
		t.Fatalf("an event the Admin switched off was counted as a failure: %+v", view)
	}
}

// TestTheDiscordPluginsWebhookURLNeverComesBackOutOfTheAPI: the webhook URL is a
// bearer credential, so it is a `secret` field — stored, usable, and never
// returned to anybody, including the Admin who typed it.
func TestTheDiscordPluginsWebhookURLNeverComesBackOutOfTheAPI(t *testing.T) {
	discord := newDiscordStandIn(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)

	installDiscordPlugin(t, srv, token)
	saveDiscordSettings(t, srv, token, map[string]any{"webhook_url": discord.webhookURL()})

	var raw json.RawMessage
	if status, body := srv.AuthGET("/api/v1/settings/plugins", token, &raw); status != http.StatusOK {
		t.Fatalf("GET plugins = %d; body: %s", status, body)
	}
	if strings.Contains(string(raw), discordWebhookToken) {
		t.Fatalf("the webhook token came back out of the settings API: %s", raw)
	}

	view := readDeclaredSettings(t, srv, token, "discord")
	if view.Settings == nil || !view.Settings.Secrets["webhook_url"] {
		t.Fatalf("the screen does not report that a webhook URL is on file: %+v", view.Settings)
	}
	if _, present := view.Settings.Values["webhook_url"]; present {
		t.Fatalf("the webhook URL is in the settings response's values: %+v", view.Settings.Values)
	}
	// And the schema the form renders is the manifest's, straight off disk.
	if len(view.SettingsSchema) != len(discordtest.Manifest(t).Settings.Fields) {
		t.Fatalf("the screen offers %d fields, the manifest declares %d",
			len(view.SettingsSchema), len(discordtest.Manifest(t).Settings.Fields))
	}
}
