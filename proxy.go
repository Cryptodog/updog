package main

import (
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

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
	if config().UseXForwardedFor {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		ip = forwarded[len(forwarded)-1]
	} else {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}

	header := http.Header{}
	header.Add("Origin", r.Header.Get("Origin"))
	header.Add("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))

	upstream, resp, err := websocket.DefaultDialer.Dial(config().UpstreamWebsocketURL, header)
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

	downstream.SetReadLimit(config().MaxMessageSize)

	// XXX: chan buffer set to (# chan writes - 1) to prevent blocked goroutines.
	errc := make(chan error, 5)
	done := make(chan bool)

	go func() {
		dmByteCount := 0
		byteCount := 0
		msgCount := 0
		dmCount := 0
		var startedCountingAt time.Time
		measureLock := sync.Mutex{}

		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(time.Duration(config().RateMeasurePeriod) * time.Second):
					measureLock.Lock()
					dmByteCount = 0
					dmCount = 0
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

				dmSize := dmBytes(string(msg))
				if dmSize > 0 {
					dmCount++
					dmByteCount += dmSize
				}

				msgCount++

				mc := msgCount
				dc := dmCount
				volume := float64(byteCount)
				dmVolume := float64(dmByteCount)
				period := time.Since(startedCountingAt)
				measureLock.Unlock()

				// XXX: magic
				if mc > config().MaxMessageCt {
					errc <- fmt.Errorf("sending messages too fast!")
					break
				}

				rate := volume / period.Seconds()
				dmRate := dmVolume / period.Seconds()

				shouldBeThrottled := (rate > float64(config().RateLimit) || dmRate > float64(config().DMRateLimit) || dc > config().MaxDMCt)

				if shouldBeThrottled {
					throttledConnectionsPerIPLock.Lock()
					throttledConnectionsPerIP[ip]++
					tc := throttledConnectionsPerIP[ip]
					throttledConnectionsPerIPLock.Unlock()

					// Throttle down to the specified rate limit.
					throttleFor := time.Duration(
						math.Abs(
							(float64(time.Second)*volume)/float64(config().RateLimit) - float64(period)))

					if dc > config().MaxDMCt {
						throttleFor += (time.Duration(dc) * 100 * time.Millisecond)
					}

					time.Sleep(throttleFor)

					throttledConnectionsPerIPLock.Lock()
					if throttledConnectionsPerIP[ip] > 0 {
						throttledConnectionsPerIP[ip]--
					} else {
						// This invariant should never be violated.
						// XXX: remove after testing
						panic("throttledConnectionsPerIP[ip] <= 0")
					}
					throttledConnectionsPerIPLock.Unlock()

					if tc > config().MaxThrottledConnectionsPerIP {
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
				case <-time.After(time.Duration(config().RateMeasurePeriod) * time.Second):
					measureLock.Lock()

					// XXX: magic
					if (float64(byteCount) / time.Since(startedCountingAt).Seconds()) > float64(8*config().RateLimit) {
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
				if startDropping && float64(len(msg)) > (float64(config().RateLimit)/100) {
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
	http.HandleFunc(config().WebsocketEndpoint, proxy)
	log.Fatal(http.ListenAndServe(config().ListenAddress, nil))
}
