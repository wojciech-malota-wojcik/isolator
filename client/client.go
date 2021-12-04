package client

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/ridge/must"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
)

type message struct {
	Type    string
	Message json.RawMessage
}

// New creates new client
func New(receiver io.Reader, sender io.Writer) *Client {
	return &Client{
		decoder: json.NewDecoder(receiver),
		encoder: json.NewEncoder(sender),
	}
}

// Client is the client for connection between executor and peer
type Client struct {
	decoder *json.Decoder
	encoder *json.Encoder
}

// Send sends message
func (c *Client) Send(msg interface{}) error {
	return c.encoder.Encode(message{
		Type:    reflect.TypeOf(msg).Name(),
		Message: must.Bytes(json.Marshal(msg)),
	})
}

// Receive receives message
func (c *Client) Receive() (interface{}, error) {
	var msg message
	if err := c.decoder.Decode(&msg); err != nil {
		return nil, err
	}
	internalMsg, err := typeToInstance(msg.Type)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(msg.Message, internalMsg); err != nil {
		return nil, err
	}
	return reflect.ValueOf(internalMsg).Elem().Interface(), nil
}

func typeToInstance(tName string) (interface{}, error) {
	var msg interface{}
	switch tName {
	case "Execute":
		msg = &wire.Execute{}
	case "Completed":
		msg = &wire.Completed{}
	case "Log":
		msg = &wire.Log{}
	default:
		return nil, fmt.Errorf("unrecognized type: %s", tName)
	}
	return msg, nil
}
