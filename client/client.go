package client

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"

	"github.com/ridge/must"
	"github.com/wojciech-malota-wojcik/isolator/executor/wire"
)

type message struct {
	Type    string
	Message json.RawMessage
}

// New creates new client
func New(conn net.Conn) *Client {
	return &Client{
		conn:    conn,
		decoder: json.NewDecoder(conn),
		encoder: json.NewEncoder(conn),
	}
}

// Client is the client for connection between executor and peer
type Client struct {
	conn    net.Conn
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
	switch tName {
	case "Execute":
		return &wire.Execute{}, nil
	case "Completed":
		return &wire.Completed{}, nil
	default:
		return nil, fmt.Errorf("unrecognized type: %s", tName)
	}
}
