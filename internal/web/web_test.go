package web_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"hearth/internal/core"
	"hearth/internal/daemon"
	"hearth/internal/testfakes"
	"hearth/internal/web"
)

const token = "0123456789abcdef0123456789abcdef"

// serve runs the web handler over a daemon serving one session.
func serve(t *testing.T) *httptest.Server {
	t.Helper()
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	srv := httptest.NewServer(web.Handler(token, daemon.Version, func() (net.Conn, error) { return h.Conn(t), nil }))
	t.Cleanup(srv.Close)
	return srv
}

// pair submits code the way the page does and returns the cookies the server set.
func pair(t *testing.T, srv *httptest.Server, code string) []*http.Cookie {
	t.Helper()
	res, err := http.PostForm(srv.URL+"/pair", url.Values{"token": {code}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.Cookies()
}

// socket opens the app's WebSocket as a page served from origin would, sending cookies.
func socket(t *testing.T, srv *httptest.Server, origin string, cookies []*http.Cookie) (*websocket.Conn, error) {
	t.Helper()
	cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", origin)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		cfg.Header.Add("Cookie", c.Name+"="+c.Value)
	}
	return websocket.DialConfig(cfg)
}

// frame is one protocol message as the page reads it.
type frame struct {
	Snapshot *struct{ Sessions []core.Session } `json:"snapshot"`
	Change   *struct{ Session *core.Session }   `json:"change"`
	Reply    *struct {
		ID  int
		Err string
	} `json:"reply"`
}

func TestPairedPageDrivesTheDaemon(t *testing.T) {
	// given
	// ... a page paired with the token, its socket open from the server's own origin
	srv := serve(t)
	ws, err := socket(t, srv, srv.URL, pair(t, srv, token))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	// when
	// ... it says hello and renames the session
	websocket.Message.Send(ws, `{"hello":{"version":"`+daemon.Version+`"}}`)
	websocket.Message.Send(ws, `{"request":{"id":1,"method":"rename","args":{"session":"local_1","title":"render markdown"}}}`)

	// then
	// ... it reads the snapshot, the renamed session and the reply, one message per frame
	var snap, change, reply frame
	for _, f := range []*frame{&snap, &change, &reply} {
		if err := websocket.JSON.Receive(ws, f); err != nil {
			t.Fatal(err)
		}
	}
	if snap.Snapshot.Sessions[0].Title != "render" || change.Change.Session.Title != "render markdown" ||
		reply.Reply.ID != 1 || reply.Reply.Err != "" {
		t.Fatalf("snapshot %+v, change %+v, reply %+v", snap.Snapshot, change.Change, reply.Reply)
	}
}

func TestPageWithTheWrongCodeCannotOpenTheSocket(t *testing.T) {
	// given
	// ... a page that tried to pair with the wrong code
	srv := serve(t)
	cookies := pair(t, srv, "fedcba9876543210fedcba9876543210")

	// when
	// ... it opens the socket with whatever cookies it was given
	_, err := socket(t, srv, srv.URL, cookies)

	// then
	// ... it was given none, and the socket is refused
	if len(cookies) != 0 || err == nil {
		t.Fatalf("cookies %v, socket error %v", cookies, err)
	}
}

func TestSocketFromAnotherOriginIsRefused(t *testing.T) {
	// given
	// ... a paired browser on another site's page under the same domain, which gets the cookie
	srv := serve(t)
	cookies := pair(t, srv, token)

	// when
	// ... that page opens hearth's socket
	_, err := socket(t, srv, "http://nea.home.example", cookies)

	// then
	// ... the socket is refused
	if err == nil {
		t.Fatal("a page from another origin opened the socket")
	}
}
