// Package nxbt implements core.InputTarget by bridging to the NXBT Python
// daemon over a socket (TCP or Unix) using newline-delimited JSON. This file
// contains the protocol wire types shared by both the connection layer and
// tests; it carries no build constraints and is safe to import on any platform.
package nxbt

import (
	"encoding/json"
	"fmt"
)

// msgType is the discriminator field in every wire message.
type msgType string

const (
	msgTypeInput    msgType = "input"
	msgTypePing     msgType = "ping"
	msgTypeStatus   msgType = "status"
	msgTypePong     msgType = "pong"
	msgTypeInputLat msgType = "input_lat"
)

// wireState maps to the daemon's reported connection state string.
type wireState string

const (
	wireStateConnecting   wireState = "connecting"
	wireStateConnected    wireState = "connected"
	wireStateDisconnected wireState = "disconnected"
)

// ---- outbound (Go → daemon) -----------------------------------------------

// inputMsg is sent by Go to forward one controller snapshot to the daemon.
// All fields are required; the daemon uses zero values for omitted axes.
//
// Wire example:
//
//	{"type":"input","buttons":1,"lx":0.5,"ly":-0.5,"rx":0,"ry":0}
type inputMsg struct {
	Type    msgType `json:"type"`
	Buttons uint32  `json:"buttons"`
	LX      float32 `json:"lx"`
	LY      float32 `json:"ly"`
	RX      float32 `json:"rx"`
	RY      float32 `json:"ry"`
}

// pingMsg is sent periodically by Go to verify the socket is alive.
//
// Wire example:
//
//	{"type":"ping"}
type pingMsg struct {
	Type msgType `json:"type"`
}

// ---- inbound (daemon → Go) -------------------------------------------------

// statusMsg is sent by the daemon whenever the Bluetooth link state changes.
//
// Wire example:
//
//	{"type":"status","state":"connected","detail":"paired to 01:23:45:67:89:AB"}
type statusMsg struct {
	Type   msgType   `json:"type"`
	State  wireState `json:"state"`
	Detail string    `json:"detail,omitempty"`
}

// pongMsg is the daemon's reply to a ping.
//
// Wire example:
//
//	{"type":"pong"}
type pongMsg struct {
	Type msgType `json:"type"`
}

// inputLatMsg reports the daemon-local elapsed time from receiving an input
// message to apply_input returning. US is microseconds.
//
// Wire example:
//
//	{"type":"input_lat","us":230}
type inputLatMsg struct {
	Type msgType `json:"type"`
	US   int64   `json:"us"`
}

// envelope is used only to sniff the "type" field before full decoding.
type envelope struct {
	Type msgType `json:"type"`
}

// encodeInput serialises an inputMsg to a newline-terminated JSON line.
func encodeInput(m inputMsg) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}
	return append(b, '\n'), nil
}

// encodePing serialises a ping to a newline-terminated JSON line.
func encodePing() ([]byte, error) {
	b, err := json.Marshal(pingMsg{Type: msgTypePing})
	if err != nil {
		return nil, fmt.Errorf("marshal ping: %w", err)
	}
	return append(b, '\n'), nil
}

// decodeInbound parses one line from the daemon.  It returns one of:
// statusMsg, pongMsg, or an error.
func decodeInbound(line []byte) (interface{}, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	switch env.Type {
	case msgTypeStatus:
		var m statusMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode status: %w", err)
		}
		return m, nil
	case msgTypePong:
		return pongMsg{Type: msgTypePong}, nil
	case msgTypeInputLat:
		var m inputLatMsg
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("decode input latency: %w", err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unknown inbound type %q", env.Type)
	}
}
