package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

// liveBody is the shape of GET /api/live's data field.
type liveBody struct {
	WindowSeconds int64                    `json:"window_seconds"`
	ServerTime    int64                    `json:"server_time"`
	Series        map[string][]liveTestPnt `json:"series"`
}

type liveTestPnt struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

func readLive(t *testing.T, a *API, query string) (int, liveBody) {
	t.Helper()
	w := adminReq(t, a.Handler(), http.MethodGet, "/api/live?"+query, "")
	var env struct {
		Data liveBody `json:"data"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode live body: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, env.Data
}

// The live view exists because the mother keeps no raw tier: a push has to be
// readable at the resolution it arrived at, which no rollup can give back.
func TestIngestFeedsTheLiveSeries(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", Samples: map[string]float64{"cpu.usage": 12.5, "memory.usage": 40},
	})

	code, body := readLive(t, a, fmt.Sprintf("server_id=%d&metric=cpu.usage,memory.usage", srv.ID))
	if code != http.StatusOK {
		t.Fatalf("live read: %d", code)
	}
	if len(body.Series["cpu.usage"]) != 1 || body.Series["cpu.usage"][0].Value != 12.5 {
		t.Fatalf("cpu series = %+v", body.Series["cpu.usage"])
	}
	if len(body.Series["memory.usage"]) != 1 {
		t.Fatalf("memory series = %+v", body.Series["memory.usage"])
	}
	if body.WindowSeconds != store.DefaultLiveWindowMinutes*60 {
		t.Fatalf("window_seconds = %d, want the %d-minute default",
			body.WindowSeconds, store.DefaultLiveWindowMinutes)
	}
}

// A metric with nothing in the window is an empty list, never a missing key:
// the panel maps over each requested metric and a null would be a render
// crash on a server that simply has that collector switched off.
func TestLiveAlwaysAnswersEveryRequestedMetric(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", Samples: map[string]float64{"cpu.usage": 1},
	})

	_, body := readLive(t, a, fmt.Sprintf("server_id=%d&metric=cpu.usage,disk.usage", srv.ID))
	series, ok := body.Series["disk.usage"]
	if !ok {
		t.Fatalf("requested metric missing from the response: %+v", body.Series)
	}
	if series == nil || len(series) != 0 {
		t.Fatalf("empty metric = %+v, want an empty list", series)
	}
}

// A server the mother has never heard of is answered as empty rather than as
// an error: the panel polls this on a timer and a deleted server would
// otherwise turn into an error banner instead of an empty chart.
func TestLiveUnknownServerIsEmpty(t *testing.T) {
	a, _ := setup(t)
	code, body := readLive(t, a, "server_id=987654&metric=cpu.usage")
	if code != http.StatusOK {
		t.Fatalf("unknown server: %d", code)
	}
	if len(body.Series["cpu.usage"]) != 0 {
		t.Fatalf("unknown server returned data: %+v", body.Series)
	}
}

func TestLiveRejectsBadRequests(t *testing.T) {
	a, _ := setup(t)
	for name, query := range map[string]string{
		"no server_id":      "metric=cpu.usage",
		"bad server_id":     "server_id=abc&metric=cpu.usage",
		"no metric":         "server_id=1",
		"empty metric list": "server_id=1&metric=,",
		"too many metrics":  "server_id=1&metric=" + strings.Repeat("cpu.usage,", maxLiveMetrics+1),
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := readLive(t, a, query); code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d", code)
			}
		})
	}
}

// The fleet table renders a CPU/RAM column per row and the overview page
// renders one per group. Embedding the newest values in the list every poll
// already reads keeps that to one request instead of one per server.
func TestListServersEmbedsLatestSamples(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", Samples: map[string]float64{"cpu.usage": 33, "memory.usage": 71},
	})

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var body struct {
		Data []struct {
			Latest   map[string]float64 `json:"latest"`
			LatestTS int64              `json:"latest_ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("fleet = %s", w.Body.String())
	}
	if body.Data[0].Latest["cpu.usage"] != 33 || body.Data[0].Latest["memory.usage"] != 71 {
		t.Fatalf("latest = %+v", body.Data[0].Latest)
	}
	if body.Data[0].LatestTS == 0 {
		t.Fatal("latest_ts must carry when the values are from")
	}
}

// A server that has never pushed must still render: `latest` is an object,
// never null, so the panel can index it without a guard.
func TestListServersLatestIsAlwaysAnObject(t *testing.T) {
	a, st := setup(t)
	st.AddServer("never-pushed")

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	if !strings.Contains(w.Body.String(), `"latest":{}`) {
		t.Fatalf("want an empty object for a server with no samples: %s", w.Body.String())
	}
}

// The window is what the operator configures, so the store has to follow the
// setting — not only for new pushes but for what is already held.
func TestSettingsApplyTheLiveWindow(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":10,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75,"live_window_minutes":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("settings save: %d %s", w.Code, w.Body)
	}
	if got := a.live.Window().Minutes(); got != 3 {
		t.Fatalf("live window = %v minutes, want 3", got)
	}
}

// Unlike every retention field, omitting the live window is allowed: it
// deletes nothing, and requiring it would 400 every existing caller —
// including a deployed panel — the moment this mother ships.
func TestSettingsLiveWindowIsOptionalAndPreserved(t *testing.T) {
	a, st := setup(t)
	adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":10,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75,"live_window_minutes":7}`)

	w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":20,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a payload without the live window must still save: %d %s", w.Code, w.Body)
	}
	got, _ := st.GetSettings()
	if got.LiveWindowMinutes != 7 {
		t.Fatalf("live window = %d, want the stored 7 to survive an older payload", got.LiveWindowMinutes)
	}
	if got.Interval != 20 {
		t.Fatalf("interval = %d, want the payload's 20", got.Interval)
	}
}

// The window is the mother's memory budget: an operator must not be able to
// ask for a day of 2-second samples for the whole fleet.
func TestSettingsRejectLiveWindowOutOfBounds(t *testing.T) {
	for _, minutes := range []string{"0", "-5", "1441", "61"} {
		a, _ := setup(t)
		w := adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
			`{"interval":10,"heartbeat_miss_threshold":3,"retention_1m_days":15,"retention_1h_days":75,"live_window_minutes":`+minutes+`}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("live_window_minutes=%s must 400, got %d", minutes, w.Code)
		}
	}
}

// seedLive puts points straight into the in-memory store. Ingest stamps a push
// with its own time.Now and refuses a second one inside the same second (and
// inside the 2s rate limit), so building a multi-point series through the HTTP
// path would mean sleeping. The store is what these tests are about.
func seedLive(a *API, serverID int64, metric string, base int64, values ...float64) {
	for i, v := range values {
		a.live.Add(serverID, base+int64(i), map[string]float64{metric: v})
	}
}

// The polling reader's steady state: it holds the window already and asks only
// for what arrived since. Anything else re-sends an hour of samples every few
// seconds for as long as the tab is open.
func TestLiveSinceReturnsOnlyNewerPoints(t *testing.T) {
	a, _ := setup(t)
	base := time.Now().Unix() - 30
	seedLive(a, 1, "cpu.usage", base, 1, 2, 3)

	_, body := readLive(t, a, fmt.Sprintf("server_id=1&metric=cpu.usage&since=%d", base+1))
	got := body.Series["cpu.usage"]
	if len(got) != 1 || got[0].Value != 3 || got[0].TS != base+2 {
		t.Fatalf("since=%d returned %+v, want only the newest point", base+1, got)
	}
}

// A first poll holds nothing, so it sends no `since` at all — and must get the
// whole window, exactly as it did before the parameter existed.
func TestLiveWithoutSinceReturnsTheWholeWindow(t *testing.T) {
	a, _ := setup(t)
	base := time.Now().Unix() - 30
	seedLive(a, 1, "cpu.usage", base, 1, 2, 3)

	_, body := readLive(t, a, "server_id=1&metric=cpu.usage")
	if len(body.Series["cpu.usage"]) != 3 {
		t.Fatalf("no since returned %+v, want the whole window", body.Series["cpu.usage"])
	}
}

// Nothing new is an empty list, not a repeat of the last point: a poll landing
// between two pushes is the common case, and duplicating the newest sample
// would draw a flat step onto the chart on every quiet tick.
func TestLiveSinceAtTheNewestPointReturnsNothing(t *testing.T) {
	a, _ := setup(t)
	base := time.Now().Unix() - 30
	seedLive(a, 1, "cpu.usage", base, 1, 2, 3)

	_, body := readLive(t, a, fmt.Sprintf("server_id=1&metric=cpu.usage&since=%d", base+2))
	series, ok := body.Series["cpu.usage"]
	if !ok || series == nil {
		t.Fatalf("a caught-up reader must still get the key as an empty list: %+v", body.Series)
	}
	if len(series) != 0 {
		t.Fatalf("since at the newest point returned %+v, want nothing", series)
	}
}

// Validated at the boundary like server_id. A `since` the caller fat-fingered
// must not read as 0 and silently turn every poll back into a full window
// read, which is the failure the parameter exists to prevent.
func TestLiveRejectsBadSince(t *testing.T) {
	a, _ := setup(t)
	for name, query := range map[string]string{
		"not a number": "server_id=1&metric=cpu.usage&since=abc",
		"negative":     "server_id=1&metric=cpu.usage&since=-5",
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := readLive(t, a, query); code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d", code)
			}
		})
	}
}

// Points are stamped with the MOTHER's clock, so a reader that slices "the
// last 5 minutes" against its own browser clock slices the wrong window the
// moment the two disagree. The answer carries the clock the timestamps came
// from.
func TestLiveCarriesTheMothersClock(t *testing.T) {
	a, _ := setup(t)
	before := time.Now().Unix()
	_, body := readLive(t, a, "server_id=1&metric=cpu.usage")
	after := time.Now().Unix()

	if body.ServerTime < before || body.ServerTime > after {
		t.Fatalf("server_time = %d, want it inside [%d, %d]", body.ServerTime, before, after)
	}
}
