package main

import (
	"github.com/Cryptodog/go-cryptodog/xmpp"
)

// Checks if a XMPP packet contains a direct message. if so, it returns the byte length of the message.
// if not, zero
func dmBytes(data string) int {
	content, _ := xmpp.Parse(string(data))
	if content != nil {
		switch c := content.(type) {
		case xmpp.Message:
			if c.Type == "chat" {
				// is a DM
				return len(c.Body)
			}
		}
	}
	return 0
}
