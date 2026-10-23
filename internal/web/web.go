// Package web serves hearth to a browser: the app, embedded in the binary, and a WebSocket
// per tab that relays the daemon's protocol, one message per frame. Where it listens and what
// sits in front of it (TLS, a proxy, a tailnet) is the user's choice, so it trusts none of
// that: a browser gets in only by pairing with the token.
package web

import (
	"bufio"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"golang.org/x/net/websocket"

	"hearth/pkg/random"
)

//go:embed static
var static embed.FS

const (
	cookie   = "hearth"
	minToken = 32 // hex digits: 128 bits
)

// Handler serves the app, pairing and the socket. version is the daemon protocol the page
// speaks; dial connects to the daemon, once per open tab.
func Handler(token, version string, dial func() (net.Conn, error)) http.Handler {
	if len(token) < minToken {
		panic("web: pairing token too short") // an empty one would admit an empty cookie
	}
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	page := template.Must(template.ParseFS(files, "index.html"))
	paired := func(r *http.Request) bool {
		c, err := r.Cookie(cookie)
		return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
	}
	ws := websocket.Server{
		// a page on another origin of the same site, say another app behind the same proxy,
		// can open this socket, and the browser sends it our cookie
		Handshake: func(c *websocket.Config, r *http.Request) error {
			origin, err := websocket.Origin(c, r)
			if err != nil || origin == nil || origin.Host != r.Host {
				return errors.New("cross-origin socket")
			}
			return nil
		},
		Handler: func(c *websocket.Conn) { relay(c, dial) },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, version)
	})
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) {
		if !paired(r) {
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(token)) != 1 {
			http.Error(w, "wrong pairing code", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookie, Value: token, Path: "/", MaxAge: 400 * 24 * 60 * 60,
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
			Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if !paired(r) {
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		ws.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a cached page would speak an older build's protocol; a framing page could click "Yes"
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

// relay pipes one tab to a daemon connection of its own until either side hangs up: each
// frame the page sends is one line to the daemon, and each line back is one frame.
func relay(ws *websocket.Conn, dial func() (net.Conn, error)) {
	conn, err := dial()
	if err != nil {
		log.Printf("web: daemon: %v", err)
		return
	}
	defer conn.Close()
	go func() {
		defer conn.Close() // the tab went away, so the daemon sees its client leave
		for {
			var b []byte
			if err := websocket.Message.Receive(ws, &b); err != nil {
				return
			}
			// the daemon reads a stream, so a partial message would run into the next frame
			if !json.Valid(b) {
				return
			}
			if _, err := conn.Write(append(b, '\n')); err != nil {
				return
			}
		}
	}()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		if err := websocket.Message.Send(ws, string(line)); err != nil {
			return
		}
	}
}

// Token reads the pairing token at path, making one the first time. Deleting the file and
// restarting unpairs every browser.
func Token(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t := random.Hex(minToken)
		return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
	}
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < minToken {
		return "", fmt.Errorf("%s holds too short a token; delete it to make a new one", path)
	}
	return t, nil
}
