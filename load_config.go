package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"sync"
	"time"
)

const configFile = "config.json"

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

	// The maximum allowed rate, in bytes per second (B/s), for direct messages coming from clients
	DMRateLimit int `json:"dm_rate_limit"`

	// The maximum number of messages that can be sent in a 5 second period
	MaxMessageCt int `json:"max_message_ct"`

	// The maximum number of direct messages that can be sent in a 5 second period
	MaxDMCt int `json:"max_dm_ct"`

	// The period, in seconds, within which traffic rates are calculated.
	// A value that seems to work well is 5.
	// TODO: formalize this
	RateMeasurePeriod int `json:"rate_measure_period"`
}

var (
	tmpCfgLock  = new(sync.Mutex)
	tmpCfg      Config
	lastCfgLoad = time.Now().Add(-4 * time.Second)
)

// Temporarily caches the config file to reduce disk I/O, although this probably isn't totally necessary.
func config() Config {
	if time.Since(lastCfgLoad) < 2*time.Second {
		return tmpCfg
	}

	tmpCfgLock.Lock()
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Fatal(err)
	}

	var cfg Config
	err = json.Unmarshal(b, &cfg)
	if err != nil {
		log.Fatal(err)
	}

	// set reasonable parameters if they are zero
	requireAtLeast(&cfg.MaxDMCt, 25)
	requireAtLeast(&cfg.MaxMessageCt, 50)

	tmpCfg = cfg
	lastCfgLoad = time.Now()
	tmpCfgLock.Unlock()

	return cfg
}

func requireAtLeast(ptr *int, data int) {
	if *ptr == 0 {
		*ptr = data
	}
}
