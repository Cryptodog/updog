package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	ListenAddress     string `json:"listen_address"`
	WebsocketEndpoint string `json:"websocket_endpoint"`

	// WebSocket URL of XMPP server.
	UpstreamWebsocketURL string `json:"upstream_websocket_url"`

	// For when we're behind another proxy, such as nginx.
	UseXForwardedFor bool `json:"use_x_forwarded_for"`

	// How many connections per IP are allowed to max out the rate limit at any given time?
	MaxThrottledConnectionsPerIP int `json:"max_throttled_connections_per_ip"`

	// The maxiumum allowed size, in bytes, for incoming WebSocket messages from clients.
	// Offending clients will have their connection terminated.
	MaxMessageSize int64 `json:"max_message_size"`

	// The maximum allowed rate, in bytes per second (B/s), for incoming traffic from clients.
	// Does not apply to WebSocket control messages, such as ping and close.
	RateLimit int `json:"rate_limit"`

	// The period, in seconds, within which traffic rates are calculated.
	// A value that seems to work well is 5.
	// TODO: formalize this
	RateMeasurePeriod int `json:"rate_measure_period"`
}

const configFile = "config.json"

var config Config

var upgrader = websocket.Upgrader{
	// We don't care about the origin.
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var throttledConnectionsPerIP = make(map[string]int)
var throttledConnectionsPerIPLock = sync.Mutex{}

func isControlMessage(messageType int) bool {
	return (messageType != websocket.TextMessage) && (messageType != websocket.BinaryMessage)
}

func proxy(w http.ResponseWriter, r *http.Request) {
	var ip string

	// TODO: verify this works for IPv6
	if config.UseXForwardedFor {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		ip = forwarded[len(forwarded)-1]
	} else {
		ip = strings.Split(r.RemoteAddr, ":")[0]
	}

	header := http.Header{}
	header.Add("Origin", r.Header.Get("Origin"))
	header.Add("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))

	upstream, resp, err := websocket.DefaultDialer.Dial(config.UpstreamWebsocketURL, header)
	if err != nil {
		log.Printf("[client: %v] dial upstream: %v", ip, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer upstream.Close()

	header = http.Header{}
	header.Add("Sec-WebSocket-Protocol", resp.Header.Get("Sec-WebSocket-Protocol"))

	downstream, err := upgrader.Upgrade(w, r, header)
	if err != nil {
		log.Printf("[client: %v] upgrade: %v", ip, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer downstream.Close()

	downstream.SetReadLimit(config.MaxMessageSize)

	// XXX: chan buffer set to (# chan writes - 1) to prevent blocked goroutines.
	errc := make(chan error, 5)
	done := make(chan bool)

	go func() {
		byteCount := 0
		msgCount := 0
		var startedCountingAt time.Time
		measureLock := sync.Mutex{}

		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(time.Duration(config.RateMeasurePeriod) * time.Second):
					measureLock.Lock()
					byteCount = 0
					msgCount = 0
					startedCountingAt = time.Now()
					measureLock.Unlock()
				}
			}
		}()

		for {
			mt, msg, err := downstream.ReadMessage()
			if err != nil {
				errc <- err
				break
			}

			if !isControlMessage(mt) {
				measureLock.Lock()
				byteCount += len(msg)
				msgCount++

				mc := msgCount
				volume := float64(byteCount)
				period := time.Since(startedCountingAt)
				measureLock.Unlock()

				// XXX: magic
				// TODO: make this a config option
				if mc > 100 {
					errc <- fmt.Errorf("sending messages too fast!")
					break
				}

				rate := volume / period.Seconds()

				if rate > float64(config.RateLimit) {
					throttledConnectionsPerIPLock.Lock()
					throttledConnectionsPerIP[ip]++
					tc := throttledConnectionsPerIP[ip]
					throttledConnectionsPerIPLock.Unlock()

					// Throttle down to the specified rate limit.
					time.Sleep(time.Duration((float64(time.Second)*volume)/float64(config.RateLimit)) - period)

					throttledConnectionsPerIPLock.Lock()
					if throttledConnectionsPerIP[ip] > 0 {
						throttledConnectionsPerIP[ip]--
					} else {
						// This invariant should never be violated.
						// XXX: remove after testing
						panic("throttledConnectionsPerIP[ip] <= 0")
					}
					throttledConnectionsPerIPLock.Unlock()

					if tc > config.MaxThrottledConnectionsPerIP {
						errc <- fmt.Errorf("too many throttled connections!")
						break
					}
				}
			}

			err = upstream.WriteMessage(mt, msg)
			if err != nil {
				errc <- err
				break
			}
		}
	}()

	go func() {
		byteCount := 0
		var startedCountingAt time.Time
		measureLock := sync.Mutex{}
		startDropping := false

		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(time.Duration(config.RateMeasurePeriod) * time.Second):
					measureLock.Lock()

					// XXX: magic
					if (float64(byteCount) / time.Since(startedCountingAt).Seconds()) > float64(8*config.RateLimit) {
						startDropping = true
					} else {
						startDropping = false
					}

					byteCount = 0
					startedCountingAt = time.Now()
					measureLock.Unlock()
				}
			}
		}()

		for {
			mt, msg, err := upstream.ReadMessage()
			if err != nil {
				errc <- err
				break
			}

			if !isControlMessage(mt) {
				measureLock.Lock()
				byteCount += len(msg)
				measureLock.Unlock()

				// XXX: magic
				// We're going to lose most (all?) group messages, but this will at least keep the client alive.
				// Pings, small PMs, public keys, typing notifs, etc. will go through.
				if startDropping && float64(len(msg)) > (float64(config.RateLimit)/100) {
					continue
				}
			}

			err = downstream.WriteMessage(mt, msg)
			if err != nil {
				errc <- err
				break
			}
		}
	}()

	log.Printf("[client: %v] disconnect: %v", ip, <-errc)

	throttledConnectionsPerIPLock.Lock()
	if throttledConnectionsPerIP[ip] == 0 {
		// Doesn't matter if there are other connections from this IP; zero-value takes care of it.
		delete(throttledConnectionsPerIP, ip)
	}
	throttledConnectionsPerIPLock.Unlock()

	go func() {
		// Tell our measuring routines to exit.
		done <- true
		done <- true
	}()
}

func main() {
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Fatal(err)
	}

	err = json.Unmarshal(b, &config)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc(config.WebsocketEndpoint, proxy)
	log.Fatal(http.ListenAndServe(config.ListenAddress, nil))
}
